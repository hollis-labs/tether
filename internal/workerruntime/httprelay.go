package workerruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/libs/security/secrets/tokenfile"
)

// HTTPRelay bridges one agent's MCP stdio stream to the hub's streamable-HTTP /mcp endpoint
// (CW-20261016-0160). The agent speaks line-delimited JSON-RPC on stdin/stdout; every message is POSTed
// to the hub with the session's current issuer token as a Bearer credential. The token lives in a 0600
// file the outpost rewrites in place before it expires, so it is re-read for every request: a header
// frozen into .mcp.json would go stale within minutes. The token is never put in an argument, an
// environment variable, a log line or an error message.
type HTTPRelay struct {
	// URL is the hub's /mcp endpoint. https only, except a loopback host (tests, local hubs).
	URL string
	// TokenFile is the 0600 file holding the current token.
	TokenFile string
	// Client overrides the HTTP client (tests). The default has no overall timeout because a response
	// may be a long-lived SSE stream.
	Client *http.Client
	// Stderr receives one-line diagnostics. Never a token.
	Stderr io.Writer

	mu       sync.Mutex // guards session, protocol, wg ordering of the first request
	session  string
	protocol string
	outMu    sync.Mutex
	out      io.Writer
	wg       sync.WaitGroup // in-flight forwards
	streamWG sync.WaitGroup // the standalone server-to-agent stream
	getOnce  sync.Once
}

const (
	maxRelayLine = 4 << 20 // one JSON-RPC message from the agent
	relayRetries = 1       // a 401 after rotation is retried once with the freshly read token
)

// ErrRelayURL is returned for an endpoint the relay refuses to send a credential to.
var ErrRelayURL = errors.New("mcp-http-relay: url must be https (or a loopback http url)")

func (r *HTTPRelay) validate() error {
	u, err := url.Parse(r.URL)
	if err != nil || u.Host == "" || u.User != nil {
		return ErrRelayURL
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return ErrRelayURL
}

// noRedirect makes a 30x a refused request. An MCP endpoint has no business redirecting, and Go's client
// keeps the Authorization header on a redirect that stays on the same hostname, including an https to http
// downgrade, which would send the bearer in cleartext.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (r *HTTPRelay) client() *http.Client {
	if r.Client != nil {
		// Never follow redirects, whatever client was injected; copy so the caller's is untouched.
		c := *r.Client
		c.CheckRedirect = noRedirect
		return &c
	}
	return &http.Client{CheckRedirect: noRedirect, Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 60 * time.Second, TLSHandshakeTimeout: 15 * time.Second}}
}

func (r *HTTPRelay) logf(format string, args ...any) {
	if r.Stderr != nil {
		_, _ = fmt.Fprintf(r.Stderr, "mcp-http-relay: "+format+"\n", args...)
	}
}

// Run relays until stdin closes (then ends the hub MCP session) or ctx is canceled.
func (r *HTTPRelay) Run(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	if err := r.validate(); err != nil {
		return err
	}
	if _, err := tokenfile.Read(r.TokenFile); err != nil {
		return fmt.Errorf("mcp-http-relay: session credential unavailable: %w", tokenFileErr(err))
	}
	r.out = stdout
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 0, 64<<10), maxRelayLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		msg := append([]byte(nil), line...)
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.forward(ctx, msg)
		}()
	}
	scanErr := sc.Err()
	r.wg.Wait()
	r.closeSession()
	cancel()
	r.streamWG.Wait()
	if scanErr != nil {
		return fmt.Errorf("mcp-http-relay: read from agent: %w", scanErr)
	}
	return nil
}

// tokenFileErr drops the path-bearing detail down to the sentinel the caller can act on.
func tokenFileErr(err error) error {
	for _, s := range []error{tokenfile.ErrMode, tokenfile.ErrOwner, tokenfile.ErrNotRegular, tokenfile.ErrEmpty, tokenfile.ErrTooLarge} {
		if errors.Is(err, s) {
			return s
		}
	}
	return errors.New("cannot read token file")
}

type rpcID = json.RawMessage

// requestID returns the id of a single JSON-RPC request (a message with both method and id), or nil.
func requestID(msg []byte) rpcID {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(msg, &m) != nil || m.Method == "" || len(m.ID) == 0 || string(m.ID) == "null" {
		return nil
	}
	return m.ID
}

func isInitialize(msg []byte) bool {
	var m struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(msg, &m) == nil && m.Method == "initialize"
}

// forward POSTs one message and writes whatever comes back to the agent.
func (r *HTTPRelay) forward(ctx context.Context, msg []byte) {
	id := requestID(msg)
	var lastStatus int
	var lastCode string
	for attempt := 0; attempt <= relayRetries; attempt++ {
		token, err := tokenfile.Read(r.TokenFile)
		if err != nil {
			r.fail(id, "session credential unavailable")
			return
		}
		status, code, retry := r.post(ctx, msg, token)
		if status == 0 && code == "" {
			return // delivered
		}
		lastStatus, lastCode = status, code
		if !retry {
			break
		}
		// The token may have rotated between our read and the hub's check; the file is the truth.
		if next, rerr := tokenfile.Read(r.TokenFile); rerr != nil || next == token {
			break
		}
	}
	r.fail(id, fmt.Sprintf("hub refused the request (status %d%s)", lastStatus, codeSuffix(lastCode)))
}

func codeSuffix(code string) string {
	if code == "" {
		return ""
	}
	return ", " + code
}

// post sends msg. It returns (0, "", false) when the exchange completed; otherwise the failing HTTP status, the
// hub's error code, and whether a retry with a fresh token is worthwhile.
func (r *HTTPRelay) post(ctx context.Context, msg []byte, token string) (int, string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(msg))
	if err != nil {
		return -1, "bad_request", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	r.mu.Lock()
	session, protocol := r.session, r.protocol
	r.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, "", false
		}
		// The error text of a transport failure can echo the URL but never request headers.
		r.logf("transport error posting to hub: %s", sanitizeNetErr(err))
		return -1, "transport_error", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return resp.StatusCode, hubErrorCode(resp.Body), true
	}
	if resp.StatusCode >= 300 {
		// Includes a refused redirect (ErrUseLastResponse hands us the 30x): report it, never follow it.
		code := hubErrorCode(resp.Body)
		if resp.StatusCode < 400 {
			code = "redirect_refused"
		}
		return resp.StatusCode, code, false
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		r.mu.Lock()
		r.session = sid
		r.mu.Unlock()
	}
	if resp.StatusCode == http.StatusAccepted {
		return 0, "", false
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	switch mediaType {
	case "text/event-stream":
		r.readSSE(resp.Body)
	default:
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayLine+1))
		if err != nil || len(body) > maxRelayLine {
			return -1, "bad_response", false
		}
		r.captureProtocol(msg, body)
		r.emit(body)
	}
	if isInitialize(msg) {
		r.startStandaloneStream(ctx)
	}
	return 0, "", false
}

// captureProtocol remembers the negotiated protocol version from an initialize response so later requests
// carry MCP-Protocol-Version.
func (r *HTTPRelay) captureProtocol(req, resp []byte) {
	if !isInitialize(req) {
		return
	}
	var out struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if json.Unmarshal(bytes.TrimSpace(resp), &out) == nil && out.Result.ProtocolVersion != "" {
		r.mu.Lock()
		r.protocol = out.Result.ProtocolVersion
		r.mu.Unlock()
	}
}

// readSSE forwards each `data:` event of a stream response to the agent.
func (r *HTTPRelay) readSSE(body io.Reader) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), maxRelayLine)
	var data bytes.Buffer
	flush := func() {
		if data.Len() == 0 {
			return
		}
		payload := bytes.TrimSpace(data.Bytes())
		data.Reset()
		if len(payload) > 0 && json.Valid(payload) {
			r.captureProtocolFromStream(payload)
			r.emit(payload)
		}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
}

// captureProtocolFromStream handles an initialize result delivered as an SSE event.
func (r *HTTPRelay) captureProtocolFromStream(payload []byte) {
	var out struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if json.Unmarshal(payload, &out) == nil && out.Result.ProtocolVersion != "" {
		r.mu.Lock()
		if r.protocol == "" {
			r.protocol = out.Result.ProtocolVersion
		}
		r.mu.Unlock()
	}
}

// emit writes one JSON message to the agent as a single line.
func (r *HTTPRelay) emit(payload []byte) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		return
	}
	compact.WriteByte('\n')
	r.outMu.Lock()
	defer r.outMu.Unlock()
	_, _ = r.out.Write(compact.Bytes())
}

// fail tells the agent a request could not be delivered, so it does not wait forever.
func (r *HTTPRelay) fail(id rpcID, message string) {
	r.logf("%s", message)
	if len(id) == 0 {
		return
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": "hub MCP transport: " + message}})
	if err == nil {
		r.emit(body)
	}
}

// hubErrorCode reads the hub's small JSON error body ({"error":{"code":"..."}}) without trusting its size.
func hubErrorCode(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return ""
	}
	var shaped struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &shaped) != nil {
		return ""
	}
	code := shaped.Error.Code
	if code == "" {
		code = shaped.Code
	}
	for _, c := range code {
		ok := c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !ok {
			return ""
		}
	}
	return code
}

// sanitizeNetErr keeps only the operation and the underlying cause, never a URL with a query or any header.
func sanitizeNetErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + ": " + ue.Err.Error()
	}
	return err.Error()
}

// startStandaloneStream opens the optional server-to-agent GET stream once a session exists. A hub that does
// not offer one (405) simply ends it.
func (r *HTTPRelay) startStandaloneStream(ctx context.Context) {
	r.getOnce.Do(func() {
		r.streamWG.Add(1)
		go func() {
			defer r.streamWG.Done()
			r.standalone(ctx)
		}()
	})
}

func (r *HTTPRelay) standalone(ctx context.Context) {
	for {
		r.mu.Lock()
		session, protocol := r.session, r.protocol
		r.mu.Unlock()
		if session == "" {
			return
		}
		token, err := tokenfile.Read(r.TokenFile)
		if err != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Mcp-Session-Id", session)
		if protocol != "" {
			req.Header.Set("MCP-Protocol-Version", protocol)
		}
		resp, err := r.client().Do(req)
		if err != nil || ctx.Err() != nil {
			return
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return // 405 (no standalone stream), 401, 403, 404: not worth looping on
		}
		r.readSSE(resp.Body)
		_ = resp.Body.Close()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// closeSession ends the hub MCP session on a clean agent exit. Best effort.
func (r *HTTPRelay) closeSession() {
	r.mu.Lock()
	session := r.session
	r.mu.Unlock()
	if session == "" {
		return
	}
	token, err := tokenfile.Read(r.TokenFile)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, r.URL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", session)
	if resp, err := r.client().Do(req); err == nil {
		_ = resp.Body.Close()
	}
}
