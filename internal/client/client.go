// Package client is the thin HTTP client used by the mux CLI to talk to a
// running muxd daemon. It wraps the transport helpers in internal/daemon
// and the JSON payload types defined in internal/api, so CLI subcommands
// stay free of HTTP plumbing.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/registry"
)

// ErrDaemonUnreachable means the daemon process is not accepting
// connections (socket missing, connection refused, or dial timeout). CLI
// callers inspect this to decide whether to fall back to a read-only
// local path (e.g., `sessions list` hitting SQLite directly).
var ErrDaemonUnreachable = errors.New("daemon unreachable")

// Client talks to a muxd daemon listening at ListenAddr.
type Client struct {
	baseURL string
	http    *http.Client
}

// MessageEnvelopeDTO is the daemon's /messages envelope shape as consumed by
// CLI/TUI clients. Address fields are kept as canonical msg:// strings so UI
// code does not need to import go-messaging just to render or reply.
type MessageEnvelopeDTO struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Channel     string          `json:"channel"`
	From        string          `json:"from"`
	To          string          `json:"to"`
	ThreadID    string          `json:"thread_id"`
	InReplyTo   string          `json:"in_reply_to"`
	Payload     json.RawMessage `json:"payload"`
	ContentType string          `json:"content_type"`
	Metadata    map[string]any  `json:"metadata"`
	CreatedAt   time.Time       `json:"created_at"`
	DeliveredAt *time.Time      `json:"delivered_at"`
	ConsumedAt  *time.Time      `json:"consumed_at"`
	ReadAt      *time.Time      `json:"read_at,omitempty"`
	ArchivedAt  *time.Time      `json:"archived_at,omitempty"`
	CanceledAt  *time.Time      `json:"canceled_at,omitempty"`
	Subject     string          `json:"subject,omitempty"`
	Body        string          `json:"body,omitempty"`
}

type messageListResponse = MessageListResult

// MessageListResult is the daemon's paginated message-list response.
type MessageListResult struct {
	Messages []MessageEnvelopeDTO `json:"messages"`
	Total    int                  `json:"total,omitempty"`
	Limit    int                  `json:"limit,omitempty"`
	Offset   int                  `json:"offset,omitempty"`
}

// MessageSendRequest is the JSON body accepted by POST /messages.
type MessageSendRequest struct {
	Kind        string            `json:"kind"`
	Channel     string            `json:"channel,omitempty"`
	From        string            `json:"from"`
	To          string            `json:"to"`
	ThreadID    string            `json:"thread_id,omitempty"`
	InReplyTo   string            `json:"in_reply_to,omitempty"`
	Payload     json.RawMessage   `json:"payload,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// MessageNotifyRequest is the JSON body accepted by POST /messages/notify.
type MessageNotifyRequest struct {
	MessageSendRequest
	Urgency   string `json:"urgency,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Wake      *bool  `json:"wake,omitempty"`
	WakeText  string `json:"wake_text,omitempty"`
}

// MessageNotifyResult is the daemon's /messages/notify response.
type MessageNotifyResult struct {
	Message       MessageEnvelopeDTO `json:"message"`
	UnreadCount   int                `json:"unread_count"`
	WakeAttempted bool               `json:"wake_attempted"`
	WakeDelivered bool               `json:"wake_delivered"`
	SessionID     string             `json:"session_id,omitempty"`
	WakeError     string             `json:"wake_error,omitempty"`
	// WakeReason carries an observational, non-error wake disposition
	// (T06, messaging vNext): "busy", "offline", "offline-race",
	// "stale-generation", or "claim-unavailable". The delivery was
	// released for retry via the daemon's shared pump in every one of
	// these cases, not lost. Distinct from WakeError, which is reserved
	// for an actual failure (SendTurn itself erroring).
	WakeReason string `json:"wake_reason,omitempty"`
}

type AIAuditQuery struct {
	EventType  string
	Provider   string
	Model      string
	SessionID  string
	CallerID   string
	Limit      int
	Since      string
	ErrorsOnly bool
}

type AIUsageQuery struct {
	Provider  string
	Model     string
	SessionID string
	CallerID  string
	Operation string
	Since     string
}

type AIBudgetsQuery struct {
	Provider  string
	Model     string
	SessionID string
	CallerID  string
}

type EventsHistoryQuery struct {
	Scopes    []string
	Kinds     []string
	SessionID string
	SinceSeq  int64
	Cursor    int64
	Limit     int
}

// New constructs a Client for the given listen_addr. The transport handles
// unix: and tcp: schemes the same way daemon.Server accepts them.
func New(listenAddr string) *Client {
	httpClient := withTracing(daemon.DialHTTPClient(listenAddr))
	return &Client{
		baseURL: daemon.BaseURL(listenAddr),
		http:    httpClient,
	}
}

// Ping issues a short GET /health to verify the daemon is reachable. It
// returns ErrDaemonUnreachable for connection-level failures so callers
// can branch on it with errors.Is.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if isUnreachable(err) {
			return fmt.Errorf("%w: %w", ErrDaemonUnreachable, err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health status %d", resp.StatusCode)
	}
	return nil
}

// Health fetches the daemon's /health payload. Returns ErrDaemonUnreachable
// on connection failure.
func (c *Client) Health(ctx context.Context) (daemon.Health, error) {
	var h daemon.Health
	if err := c.getJSON(ctx, "/health", &h); err != nil {
		return daemon.Health{}, err
	}
	return h, nil
}

func (c *Client) AIChat(ctx context.Context, req api.ChatRequest) (api.ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return api.ChatResponse{}, fmt.Errorf("marshal ai chat request: %w", err)
	}
	longClient := *c.http
	longClient.Timeout = 0
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ai/chat", bytes.NewReader(body))
	if err != nil {
		return api.ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := longClient.Do(httpReq)
	if err != nil {
		return api.ChatResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.ChatResponse{}, readError(resp)
	}
	var out api.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return api.ChatResponse{}, fmt.Errorf("decode ai chat response: %w", err)
	}
	return out, nil
}

func (c *Client) AIEmbeddings(ctx context.Context, req api.ChatRequest) (api.ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return api.ChatResponse{}, fmt.Errorf("marshal ai embeddings request: %w", err)
	}
	longClient := *c.http
	longClient.Timeout = 0
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ai/embeddings", bytes.NewReader(body))
	if err != nil {
		return api.ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := longClient.Do(httpReq)
	if err != nil {
		return api.ChatResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.ChatResponse{}, readError(resp)
	}
	var out api.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return api.ChatResponse{}, fmt.Errorf("decode ai embeddings response: %w", err)
	}
	return out, nil
}

func (c *Client) AIPreviewRoute(ctx context.Context, req api.ChatRequest) (api.RoutePreviewResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return api.RoutePreviewResponse{}, fmt.Errorf("marshal ai route preview request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ai/routes/preview", bytes.NewReader(body))
	if err != nil {
		return api.RoutePreviewResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return api.RoutePreviewResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.RoutePreviewResponse{}, readError(resp)
	}
	var out api.RoutePreviewResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return api.RoutePreviewResponse{}, fmt.Errorf("decode ai route preview response: %w", err)
	}
	return out, nil
}

func (c *Client) AIExplainRoute(ctx context.Context, req api.ChatRequest) (api.RouteExplainResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return api.RouteExplainResponse{}, fmt.Errorf("marshal ai route explain request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ai/routes/explain", bytes.NewReader(body))
	if err != nil {
		return api.RouteExplainResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return api.RouteExplainResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.RouteExplainResponse{}, readError(resp)
	}
	var out api.RouteExplainResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return api.RouteExplainResponse{}, fmt.Errorf("decode ai route explain response: %w", err)
	}
	return out, nil
}

func (c *Client) AIProviders(ctx context.Context) (api.ListAIProvidersResponse, error) {
	var out api.ListAIProvidersResponse
	if err := c.getJSON(ctx, "/ai/providers", &out); err != nil {
		return api.ListAIProvidersResponse{}, err
	}
	return out, nil
}

func (c *Client) AIModels(ctx context.Context, providerID string) (api.ListAIModelsResponse, error) {
	path := "/ai/models"
	if providerID != "" {
		path += "?provider_id=" + url.QueryEscape(providerID)
	}
	var out api.ListAIModelsResponse
	if err := c.getJSON(ctx, path, &out); err != nil {
		return api.ListAIModelsResponse{}, err
	}
	return out, nil
}

func (c *Client) AIRoutes(ctx context.Context) (api.ListAIRoutesResponse, error) {
	var out api.ListAIRoutesResponse
	if err := c.getJSON(ctx, "/ai/routes", &out); err != nil {
		return api.ListAIRoutesResponse{}, err
	}
	return out, nil
}

func (c *Client) AIAudit(ctx context.Context, q AIAuditQuery) (api.AIAuditListResponse, error) {
	path := "/ai/audit"
	params := url.Values{}
	if q.EventType != "" {
		params.Set("event_type", q.EventType)
	}
	if q.Provider != "" {
		params.Set("provider", q.Provider)
	}
	if q.Model != "" {
		params.Set("model", q.Model)
	}
	if q.SessionID != "" {
		params.Set("session_id", q.SessionID)
	}
	if q.CallerID != "" {
		params.Set("caller_id", q.CallerID)
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Since != "" {
		params.Set("since", q.Since)
	}
	if q.ErrorsOnly {
		params.Set("errors_only", "true")
	}
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var out api.AIAuditListResponse
	if err := c.getJSON(ctx, path, &out); err != nil {
		return api.AIAuditListResponse{}, err
	}
	return out, nil
}

func (c *Client) AIUsage(ctx context.Context, q AIUsageQuery) (api.AIUsageResponse, error) {
	path := "/ai/usage"
	params := url.Values{}
	if q.Provider != "" {
		params.Set("provider", q.Provider)
	}
	if q.Model != "" {
		params.Set("model", q.Model)
	}
	if q.SessionID != "" {
		params.Set("session_id", q.SessionID)
	}
	if q.CallerID != "" {
		params.Set("caller_id", q.CallerID)
	}
	if q.Operation != "" {
		params.Set("operation", q.Operation)
	}
	if q.Since != "" {
		params.Set("since", q.Since)
	}
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var out api.AIUsageResponse
	if err := c.getJSON(ctx, path, &out); err != nil {
		return api.AIUsageResponse{}, err
	}
	return out, nil
}

func (c *Client) AIBudgets(ctx context.Context, q AIBudgetsQuery) (api.AIUsageBudgetsResponse, error) {
	path := "/ai/budgets"
	params := url.Values{}
	if q.Provider != "" {
		params.Set("provider", q.Provider)
	}
	if q.Model != "" {
		params.Set("model", q.Model)
	}
	if q.SessionID != "" {
		params.Set("session_id", q.SessionID)
	}
	if q.CallerID != "" {
		params.Set("caller_id", q.CallerID)
	}
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var out api.AIUsageBudgetsResponse
	if err := c.getJSON(ctx, path, &out); err != nil {
		return api.AIUsageBudgetsResponse{}, err
	}
	return out, nil
}

// CreateSessionWithBootPrompt POSTs /sessions with a boot_prompt override,
// replacing the catalog's static boot fragments with the provided text.
// The session is in state=created; call LaunchSession to start it.
func (c *Client) CreateSessionWithBootPrompt(ctx context.Context, launchID, bootPrompt string) (api.LaunchResponse, error) {
	body, _ := json.Marshal(api.LaunchRequest{Launch: launchID, BootPrompt: bootPrompt})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sessions", bytes.NewReader(body))
	if err != nil {
		return api.LaunchResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return api.LaunchResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusCreated {
		return api.LaunchResponse{}, readError(resp)
	}
	var res api.LaunchResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return api.LaunchResponse{}, fmt.Errorf("decode create response: %w", err)
	}
	return res, nil
}

func (c *Client) CreateSession(ctx context.Context, launchID string) (api.LaunchResponse, error) {
	body, _ := json.Marshal(api.LaunchRequest{Launch: launchID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sessions", bytes.NewReader(body))
	if err != nil {
		return api.LaunchResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return api.LaunchResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return api.LaunchResponse{}, readError(resp)
	}
	var res api.LaunchResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return api.LaunchResponse{}, fmt.Errorf("decode create response: %w", err)
	}
	return res, nil
}

// LaunchSession POSTs /sessions/{id}/launch to transition a created
// session to running. Returns the launched metadata.
func (c *Client) LaunchSession(ctx context.Context, sessionID string) (api.LaunchResponse, error) {
	// Drop the 5s transport timeout — spawning an agent process can take
	// longer than 5 seconds depending on provider startup time.
	longClient := *c.http
	longClient.Timeout = 0
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/sessions/"+url.PathEscape(sessionID)+"/launch", nil)
	if err != nil {
		return api.LaunchResponse{}, err
	}
	resp, err := longClient.Do(req)
	if err != nil {
		return api.LaunchResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.LaunchResponse{}, readError(resp)
	}
	var res api.LaunchResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return api.LaunchResponse{}, fmt.Errorf("decode launch response: %w", err)
	}
	return res, nil
}

// Launch is a convenience that creates then launches, returning the
// final response. The common case — CLI callers want one round-trip-
// per-user-action, not two.
func (c *Client) Launch(ctx context.Context, launchID string) (api.LaunchResponse, error) {
	created, err := c.CreateSession(ctx, launchID)
	if err != nil {
		return api.LaunchResponse{}, err
	}
	return c.LaunchSession(ctx, created.ID)
}

// CreateSessionWithInput POSTs /sessions with the v005-08 Agent Ops Tier-2
// caller-provided payload fields. Returns the created session in state=created;
// caller follows with LaunchSession to start it (or use LaunchWithInput for
// the combined flow).
func (c *Client) CreateSessionWithInput(ctx context.Context, req api.LaunchRequest) (api.LaunchResponse, error) {
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sessions", bytes.NewReader(body))
	if err != nil {
		return api.LaunchResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return api.LaunchResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusCreated {
		return api.LaunchResponse{}, readError(resp)
	}
	var res api.LaunchResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return api.LaunchResponse{}, fmt.Errorf("decode create response: %w", err)
	}
	return res, nil
}

// LaunchWithInput is the combined create+launch convenience for v005-08
// Tier-2 callers (analog of Launch but accepts an enriched request body).
func (c *Client) LaunchWithInput(ctx context.Context, req api.LaunchRequest) (api.LaunchResponse, error) {
	created, err := c.CreateSessionWithInput(ctx, req)
	if err != nil {
		return api.LaunchResponse{}, err
	}
	return c.LaunchSession(ctx, created.ID)
}

// ListOptions narrows a ListSessions request. Fields map 1:1 onto the
// GET /sessions query params: ?limit=, ?cursor=, ?state=. A zero-valued
// ListOptions retrieves the first default-size page with no filter.
type ListOptions struct {
	Limit  int
	Cursor string
	State  string
}

// ListResult carries the session page plus any pagination cursor the
// server emitted. NextCursor is empty when the caller has reached the
// end of the range.
type ListResult struct {
	Sessions   []api.SessionDTO
	NextCursor string
}

func (c *Client) ListSessions(ctx context.Context, opts ListOptions) (*ListResult, error) {
	u := "/sessions"
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.State != "" {
		q.Set("state", opts.State)
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var res api.ListSessionsResponse
	if err := c.getJSON(ctx, u, &res); err != nil {
		return nil, err
	}
	return &ListResult{Sessions: res.Sessions, NextCursor: res.NextCursor}, nil
}

// GetSession fetches one session by id. Returns os-style not-found semantics
// via a descriptive error when the daemon returns 404.
func (c *Client) GetSession(ctx context.Context, id string) (api.SessionDTO, error) {
	var dto api.SessionDTO
	if err := c.getJSON(ctx, "/sessions/"+url.PathEscape(id), &dto); err != nil {
		return api.SessionDTO{}, err
	}
	return dto, nil
}

// StopSession signals the daemon to terminate a running session. Returns
// nil on 204. A 404 (session not running) is surfaced as an error.
func (c *Client) StopSession(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sessions/"+url.PathEscape(id)+"/stop", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return readError(resp)
}

// ResizeSession forwards a (rows, cols) winsize update to the named
// session's PTY via POST /sessions/{id}/resize. 204 on success.
// See ADR 0014 for the contract.
func (c *Client) ResizeSession(ctx context.Context, id string, rows, cols uint16) error {
	body, _ := json.Marshal(api.ResizeRequest{Rows: rows, Cols: cols})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/sessions/"+url.PathEscape(id)+"/resize", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return readError(resp)
}

// SendInput writes data to the named session's PTY via the daemon's input
// endpoint. The body is sent raw (application/octet-stream); no framing
// or newline handling — the caller decides.
func (c *Client) SendInput(ctx context.Context, id string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/sessions/"+url.PathEscape(id)+"/input", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return readError(resp)
}

// SendTurn delivers a user message to the named session via the daemon's
// /turn endpoint. The daemon applies lifecycle-aware framing — NDJSON
// envelope for streaming-stdio, JSON-RPC turn/start for jsonrpc-stdio,
// raw write for PTY. Prefer this over SendInput for long-lived agent
// turns; SendInput stays for callers that own their own framing.
func (c *Client) SendTurn(ctx context.Context, id, text string) error {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	// Turns may block until a provider finishes a request. The default daemon
	// client timeout is intentionally short for health/catalog calls, so use an
	// unbounded transport here and let the caller's context own cancellation.
	longClient := *c.http
	longClient.Timeout = 0
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/sessions/"+url.PathEscape(id)+"/turn", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := longClient.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return readError(resp)
}

// AttachSession opens a streaming GET and copies live PTY output to w until
// ctx is canceled or the session exits (server closes the response).
// Returns nil on clean EOF (session terminated); returns a wrapped ctx.Err
// when the caller cancels.
//
// sinceSeq is a byte-offset resume hint. 0 requests the full replay
// ring (pre-resume default). Nonzero asks the server to replay only
// bytes beyond that offset — useful for reconnect after a detach
// where the client already has a partial byte-count watermark.
func (c *Client) AttachSession(ctx context.Context, id string, w io.Writer, sinceSeq int64) error {
	// Drop the 5s transport timeout — attach is long-lived.
	longClient := *c.http
	longClient.Timeout = 0
	path := c.baseURL + "/sessions/" + url.PathEscape(id) + "/attach"
	if sinceSeq > 0 {
		path += "?since_seq=" + strconv.FormatInt(sinceSeq, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := longClient.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	_, err = io.Copy(w, resp.Body)
	// ctx cancellation turns into a ctx error at the copy layer; surface it.
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return err
	}
	return nil
}

// WaitSession long-polls the daemon until the session terminates; returns
// the exit code recorded by the runtime manager. The ctx controls the
// client-side timeout; the daemon itself does not apply one.
func (c *Client) WaitSession(ctx context.Context, id string) (int, error) {
	var res api.WaitResponse
	// Override the default 5s transport timeout for this specific call so
	// long-running sessions can block.
	longClient := *c.http
	longClient.Timeout = 0
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/sessions/"+url.PathEscape(id)+"/wait", nil)
	if err != nil {
		return 0, err
	}
	resp, err := longClient.Do(req)
	if err != nil {
		return 0, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, readError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return 0, fmt.Errorf("decode wait response: %w", err)
	}
	return res.ExitCode, nil
}

// ListProjects fetches the full project catalog via GET /catalog/projects.
// No filtering or pagination — catalog sizes are small. Callers filter
// locally. Returns ErrDaemonUnreachable on connection failure.
func (c *Client) ListProjects(ctx context.Context) ([]config.Project, error) {
	var res api.ListProjectsResponse
	if err := c.getJSON(ctx, "/catalog/projects", &res); err != nil {
		return nil, err
	}
	return res.Projects, nil
}

// ListAgents fetches the full agent catalog via GET /catalog/agents.
func (c *Client) ListAgents(ctx context.Context) ([]config.Agent, error) {
	var res api.ListAgentsResponse
	if err := c.getJSON(ctx, "/catalog/agents", &res); err != nil {
		return nil, err
	}
	return res.Agents, nil
}

// ListProviders fetches the full provider catalog via GET /catalog/providers.
func (c *Client) ListProviders(ctx context.Context) ([]config.Provider, error) {
	var res api.ListProvidersResponse
	if err := c.getJSON(ctx, "/catalog/providers", &res); err != nil {
		return nil, err
	}
	return res.Providers, nil
}

// ListLaunches fetches the full launch catalog via GET /catalog/launches.
func (c *Client) ListLaunches(ctx context.Context) ([]config.Launch, error) {
	var res api.ListLaunchesResponse
	if err := c.getJSON(ctx, "/catalog/launches", &res); err != nil {
		return nil, err
	}
	return res.Launches, nil
}

// CreateCheckpoint posts a checkpoint for sessionID via POST /sessions/{id}/checkpoint.
func (c *Client) CreateCheckpoint(ctx context.Context, sessionID, status, summary string) error {
	body, _ := json.Marshal(api.CheckpointCreateRequest{Status: status, Summary: summary})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/sessions/"+url.PathEscape(sessionID)+"/checkpoint", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		return nil
	}
	return readError(resp)
}

// ResumeLogicalAgent posts a resume request for the given logical agent and
// returns the full launch response (new session ID + workspace/log/provider
// metadata) on success. v005-09 widened the return shape from string-only to
// the full envelope so MCP/ACP adapters can surface workspace + log paths
// without a follow-up GetSession round-trip.
func (c *Client) ResumeLogicalAgent(ctx context.Context, agentID string) (api.LaunchResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/logical-agents/"+url.PathEscape(agentID)+"/resume", nil)
	if err != nil {
		return api.LaunchResponse{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return api.LaunchResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return api.LaunchResponse{}, readError(resp)
	}
	var res api.LaunchResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return api.LaunchResponse{}, fmt.Errorf("decode resume response: %w", err)
	}
	return res, nil
}

// ListLogicalAgents fetches all logical agents from GET /logical-agents.
func (c *Client) ListLogicalAgents(ctx context.Context) ([]api.LogicalAgentSummary, error) {
	var res api.LogicalAgentListResponse
	if err := c.getJSON(ctx, "/logical-agents", &res); err != nil {
		return nil, err
	}
	return res.Agents, nil
}

func (c *Client) GetLogicalAgentPolicy(ctx context.Context, agentID string) (api.LogicalAgentPolicyResponse, error) {
	var res api.LogicalAgentPolicyResponse
	if err := c.getJSON(ctx, "/logical-agents/"+url.PathEscape(agentID)+"/policy", &res); err != nil {
		return api.LogicalAgentPolicyResponse{}, err
	}
	return res, nil
}

func (c *Client) UpdateLogicalAgentPolicy(ctx context.Context, policy agent.LogicalAgentPolicy) (api.LogicalAgentPolicyResponse, error) {
	body, _ := json.Marshal(api.LogicalAgentPolicyUpdateRequest{
		CheckpointPolicy: string(policy.CheckpointPolicy),
		CheckpointStatus: policy.CheckpointStatus,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		c.baseURL+"/logical-agents/"+url.PathEscape(policy.LogicalAgentID)+"/policy", bytes.NewReader(body))
	if err != nil {
		return api.LogicalAgentPolicyResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return api.LogicalAgentPolicyResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.LogicalAgentPolicyResponse{}, readError(resp)
	}
	var res api.LogicalAgentPolicyResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return api.LogicalAgentPolicyResponse{}, fmt.Errorf("decode logical agent policy response: %w", err)
	}
	return res, nil
}

// MessageInbox fetches pending messages for a recipient from GET /messages/inbox.
// The daemon currently treats inbox reads as delivery, so callers should keep
// returned messages locally if they need to continue displaying them.
//
// T05 (messaging vNext, ADR 0045): the daemon requires ?as= to equal ?to= for
// a mailbox read, so `to` doubles as the caller's identity claim here.
func (c *Client) MessageInbox(ctx context.Context, to, kind, threadID string) ([]MessageEnvelopeDTO, error) {
	params := url.Values{}
	params.Set("to", to)
	params.Set("as", to)
	if kind != "" {
		params.Set("kind", kind)
	}
	if threadID != "" {
		params.Set("thread_id", threadID)
	}
	var res messageListResponse
	if err := c.getJSON(ctx, "/messages/inbox?"+params.Encode(), &res); err != nil {
		return nil, err
	}
	return res.Messages, nil
}

// MessageList fetches messages for a recipient without marking them delivered.
//
// T05 (messaging vNext, ADR 0045): the daemon requires ?as= to equal ?to= for
// a mailbox read, so `to` doubles as the caller's identity claim here.
func (c *Client) MessageList(ctx context.Context, to, kind, threadID string, includeArchived, unreadOnly bool, limit, offset int) (MessageListResult, error) {
	params := url.Values{}
	params.Set("to", to)
	params.Set("as", to)
	if kind != "" {
		params.Set("kind", kind)
	}
	if threadID != "" {
		params.Set("thread_id", threadID)
	}
	if includeArchived {
		params.Set("include_archived", "true")
	}
	if unreadOnly {
		params.Set("unread_only", "true")
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		params.Set("offset", strconv.Itoa(offset))
	}
	var res messageListResponse
	if err := c.getJSON(ctx, "/messages/list?"+params.Encode(), &res); err != nil {
		return MessageListResult{}, err
	}
	return res, nil
}

// MessageGet fetches one message envelope. as (T05 / ADR 0045: same-host
// trust model) must be the message's sender or recipient URN.
func (c *Client) MessageGet(ctx context.Context, id, as string) (MessageEnvelopeDTO, error) {
	params := url.Values{}
	params.Set("as", as)
	var env MessageEnvelopeDTO
	if err := c.getJSON(ctx, "/messages/"+url.PathEscape(id)+"?"+params.Encode(), &env); err != nil {
		return MessageEnvelopeDTO{}, err
	}
	return env, nil
}

// MessageThread loads all messages in a Mux message thread that involve as
// as sender or recipient (T05 / ADR 0045: same-host trust model). kind, when
// non-empty, is a comma-separated kind filter (matches the HTTP route's
// ?kind= parameter); pass "" for no filter.
func (c *Client) MessageThread(ctx context.Context, threadID, as, kind string) ([]MessageEnvelopeDTO, error) {
	params := url.Values{}
	params.Set("as", as)
	if kind != "" {
		params.Set("kind", kind)
	}
	path := "/messages/thread/" + url.PathEscape(threadID) + "?" + params.Encode()
	var res messageListResponse
	if err := c.getJSON(ctx, path, &res); err != nil {
		return nil, err
	}
	return res.Messages, nil
}

// MessageSend posts a new envelope through POST /messages.
func (c *Client) MessageSend(ctx context.Context, msg MessageSendRequest) (MessageEnvelopeDTO, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return MessageEnvelopeDTO{}, fmt.Errorf("marshal message request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return MessageEnvelopeDTO{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return MessageEnvelopeDTO{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return MessageEnvelopeDTO{}, readError(resp)
	}
	var sent MessageEnvelopeDTO
	if err := json.NewDecoder(resp.Body).Decode(&sent); err != nil {
		return MessageEnvelopeDTO{}, fmt.Errorf("decode message response: %w", err)
	}
	return sent, nil
}

// MessageNotify sends an envelope and asks the daemon to wake-inject a live
// recipient session when one can be resolved.
func (c *Client) MessageNotify(ctx context.Context, msg MessageNotifyRequest) (MessageNotifyResult, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return MessageNotifyResult{}, fmt.Errorf("marshal notify request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/messages/notify", bytes.NewReader(body))
	if err != nil {
		return MessageNotifyResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return MessageNotifyResult{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return MessageNotifyResult{}, readError(resp)
	}
	var out MessageNotifyResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return MessageNotifyResult{}, fmt.Errorf("decode notify response: %w", err)
	}
	return out, nil
}

// MessageConsume marks a message consumed by the named recipient.
func (c *Client) MessageConsume(ctx context.Context, id, as string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/messages/"+url.PathEscape(id)+"/consume?as="+url.QueryEscape(as), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return readError(resp)
}

// MessageCancel cancels a pending message.
func (c *Client) MessageCancel(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/messages/"+url.PathEscape(id)+"/cancel", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return readError(resp)
}

// MessageLease is the bearer credential a claim returns and ack/nack must
// present back (T07/T08, messaging vNext). Mirrors go-messaging's
// delivery.LeaseRef on the wire without importing that package here --
// this client deliberately stays decoupled from go-messaging's own types
// (see the MessageEnvelopeDTO doc above).
type MessageLease struct {
	DeliveryID        string `json:"delivery_id"`
	AttemptID         string `json:"attempt_id"`
	LeaseToken        string `json:"lease_token"`
	BindingGeneration int64  `json:"binding_generation,omitempty"`
}

// MessageClaimResult is POST /messages/{id}/claim's response.
type MessageClaimResult struct {
	Message   MessageEnvelopeDTO `json:"message"`
	Lease     MessageLease       `json:"lease"`
	ExpiresIn int                `json:"lease_seconds"`
}

// MessageClaim durably claims a message's delivery on behalf of the
// named recipient (as) so the caller can process it on its own
// initiative rather than waiting for Tether to push a wake -- the
// published-local bridge contract (T07). holder identifies the claimant
// (defaults server-side to as when empty); leaseSeconds <= 0 uses the
// server's default.
func (c *Client) MessageClaim(ctx context.Context, id, as, holder string, leaseSeconds int) (MessageClaimResult, error) {
	body, err := json.Marshal(map[string]any{"holder": holder, "lease_seconds": leaseSeconds})
	if err != nil {
		return MessageClaimResult{}, fmt.Errorf("marshal claim request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/messages/"+url.PathEscape(id)+"/claim?as="+url.QueryEscape(as), bytes.NewReader(body))
	if err != nil {
		return MessageClaimResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return MessageClaimResult{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return MessageClaimResult{}, readError(resp)
	}
	var out MessageClaimResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return MessageClaimResult{}, fmt.Errorf("decode claim response: %w", err)
	}
	return out, nil
}

// MessageAck records stage against a lease MessageClaim returned. stage
// must be one of "host_accepted", "turn_submitted", "consumed".
func (c *Client) MessageAck(ctx context.Context, id, as string, lease MessageLease, stage string) error {
	body, err := json.Marshal(map[string]any{"lease": lease, "stage": stage})
	if err != nil {
		return fmt.Errorf("marshal ack request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/messages/"+url.PathEscape(id)+"/ack?as="+url.QueryEscape(as), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return readError(resp)
}

// MessageNack declines or fails to finish processing a claimed delivery.
// retryable schedules a retry after nextAttemptSeconds (<=0: immediately
// claimable again); retryable=false dead-letters it.
func (c *Client) MessageNack(ctx context.Context, id, as string, lease MessageLease, retryable bool, errMsg string, nextAttemptSeconds int) error {
	body, err := json.Marshal(map[string]any{
		"lease": lease, "retryable": retryable, "error": errMsg, "next_attempt_seconds": nextAttemptSeconds,
	})
	if err != nil {
		return fmt.Errorf("marshal nack request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/messages/"+url.PathEscape(id)+"/nack?as="+url.QueryEscape(as), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return readError(resp)
}

// MessageMarkRead marks a message read by the named recipient.
func (c *Client) MessageMarkRead(ctx context.Context, id, as string) error {
	return c.messageRecipientAction(ctx, id, "read", as)
}

// MessageArchive archives a message for the named recipient.
func (c *Client) MessageArchive(ctx context.Context, id, as string) error {
	return c.messageRecipientAction(ctx, id, "archive", as)
}

// MessageUnarchive restores an archived message for the named recipient.
func (c *Client) MessageUnarchive(ctx context.Context, id, as string) error {
	return c.messageRecipientAction(ctx, id, "unarchive", as)
}

// WhoAmIBinding mirrors internal/api/whoami.go's whoAmIBinding DTO.
type WhoAmIBinding struct {
	ID           string   `json:"id"`
	SessionID    string   `json:"session_id"`
	HostID       string   `json:"host_id"`
	Generation   int64    `json:"generation"`
	Capabilities []string `json:"capabilities,omitempty"`
	Visibility   string   `json:"visibility"`
}

// WhoAmISession mirrors internal/api/whoami.go's whoAmISession DTO.
type WhoAmISession struct {
	SessionID    string   `json:"session_id"`
	Host         string   `json:"host"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// WhoAmIResult is GET /whoami's response (T08, messaging vNext):
// self-discovery for a self-asserted URN (ADR 0045 -- this reports what
// Tether currently knows about that identity, it does not verify the
// claim). Profile uses registry.Profile directly (unlike the message
// DTOs, this client already depends on internal/registry for
// RegistryClient, so there is no external-dependency reason to duplicate
// its shape here).
type WhoAmIResult struct {
	URN     string             `json:"urn"`
	Profile *registry.Profile  `json:"profile,omitempty"`
	Binding *WhoAmIBinding     `json:"binding,omitempty"`
	Groups  []registry.Profile `json:"groups,omitempty"`
	Session *WhoAmISession     `json:"session,omitempty"`
}

// WhoAmI GETs /whoami?urn=<urn>.
func (c *Client) WhoAmI(ctx context.Context, urn string) (WhoAmIResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/whoami?urn="+url.QueryEscape(urn), nil)
	if err != nil {
		return WhoAmIResult{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return WhoAmIResult{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return WhoAmIResult{}, readError(resp)
	}
	var out WhoAmIResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return WhoAmIResult{}, fmt.Errorf("decode whoami response: %w", err)
	}
	return out, nil
}

// BootstrapSessionRequest is POST /bootstrap/session's request body (T08,
// messaging vNext) -- the provider-neutral local bootstrap/registration
// helper agent-setup invokes at the launch/host boundary. SessionID (the
// preassigned SESSION) is required; every other field is optional. See
// internal/api/bootstrap.go's doc comment for the full publication/
// idempotency contract.
type BootstrapSessionRequest struct {
	SessionID      string `json:"session_id"`
	Intent         string `json:"intent,omitempty"`
	LogicalAgentID string `json:"logical_agent_id,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
	ProviderID     string `json:"provider_id,omitempty"`
	Publication    string `json:"publication,omitempty"`
	HostID         string `json:"host_id,omitempty"`
	AttemptID      string `json:"attempt_id,omitempty"`
	Substrate      string `json:"substrate,omitempty"`
}

// BootstrapSessionResult is POST /bootstrap/session's response: a
// WhoAmIResult for the identity this call resolved, plus two flags
// distinguishing a fresh registration/lease from an idempotent repeat.
type BootstrapSessionResult struct {
	WhoAmIResult
	RegisteredNow bool `json:"registered_now,omitempty"`
	ReusedBinding bool `json:"reused_binding,omitempty"`
}

// BootstrapSession POSTs /bootstrap/session. Callers on an unreachable
// daemon should catch ErrDaemonUnreachable (via errors.Is on the
// returned error) and fall back to a purely local SESSION identity
// instead of failing the launch -- "bootstrap and local messaging cannot
// require online Tether" is a requirement on the CALLING side; this
// method is the online-Tether half of that story, not an offline mode
// of its own.
func (c *Client) BootstrapSession(ctx context.Context, req BootstrapSessionRequest) (BootstrapSessionResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return BootstrapSessionResult{}, fmt.Errorf("marshal bootstrap request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bootstrap/session", bytes.NewReader(body))
	if err != nil {
		return BootstrapSessionResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return BootstrapSessionResult{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return BootstrapSessionResult{}, readError(resp)
	}
	var out BootstrapSessionResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return BootstrapSessionResult{}, fmt.Errorf("decode bootstrap response: %w", err)
	}
	return out, nil
}

func (c *Client) messageRecipientAction(ctx context.Context, id, action, as string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/messages/"+url.PathEscape(id)+"/"+action+"?as="+url.QueryEscape(as), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return readError(resp)
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// readError reads a daemon error envelope from resp.Body. Returns a
// descriptive error that preserves the HTTP status + error code for
// callers that want to branch on 404 vs. 500 or on the typed code.
func readError(resp *http.Response) error {
	var env api.ErrorResponse
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return fmt.Errorf("daemon %d (%s): %s", resp.StatusCode, env.Error.Code, env.Error.Message)
	}
	return fmt.Errorf("daemon %d: %s", resp.StatusCode, string(body))
}

// QueryProxyEvents fetches MCP proxy tool call events from GET /proxy/events.
// All filter fields are optional; zero values match everything.
// Returns ErrDaemonUnreachable when the daemon is not running.
func (c *Client) QueryProxyEvents(ctx context.Context, serverID, toolName string, limit int, errorsOnly bool) ([]api.ProxyEventDTO, error) {
	u := "/proxy/events?"
	params := url.Values{}
	if serverID != "" {
		params.Set("server", serverID)
	}
	if toolName != "" {
		params.Set("tool_name", toolName)
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if errorsOnly {
		params.Set("errors_only", "true")
	}
	u += params.Encode()

	var res api.ProxyEventListResponse
	if err := c.getJSON(ctx, u, &res); err != nil {
		return nil, wrapIfUnreachable(err)
	}
	return res.Events, nil
}

// EventsHistory fetches durable daemon/session/broker event history from
// GET /events with optional filters. Results are newest first.
func (c *Client) EventsHistory(ctx context.Context, q EventsHistoryQuery) (api.EventListResponse, error) {
	path := "/events"
	params := url.Values{}
	for _, scope := range q.Scopes {
		if strings.TrimSpace(scope) != "" {
			params.Add("scope", scope)
		}
	}
	for _, kind := range q.Kinds {
		if strings.TrimSpace(kind) != "" {
			params.Add("kind", kind)
		}
	}
	if q.SessionID != "" {
		params.Set("session_id", q.SessionID)
	}
	if q.SinceSeq > 0 {
		params.Set("since_seq", strconv.FormatInt(q.SinceSeq, 10))
	}
	if q.Cursor > 0 {
		params.Set("cursor", strconv.FormatInt(q.Cursor, 10))
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var res api.EventListResponse
	if err := c.getJSON(ctx, path, &res); err != nil {
		return api.EventListResponse{}, wrapIfUnreachable(err)
	}
	return res, nil
}

// wrapIfUnreachable annotates connection-refused / socket-missing errors
// so CLI callers can fall back to a local path.
func wrapIfUnreachable(err error) error {
	if isUnreachable(err) {
		return fmt.Errorf("%w: %w", ErrDaemonUnreachable, err)
	}
	return err
}

func isUnreachable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Dial errors against a non-existent socket or stopped TCP listener
		// land here with opErr.Op == "dial".
		if opErr.Op == "dial" {
			return true
		}
	}
	// Fall back to substring sniff for wrapped forms.
	s := err.Error()
	return strings.Contains(s, "connection refused") || strings.Contains(s, "no such file or directory")
}
