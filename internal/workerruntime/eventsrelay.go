package workerruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/ingest"

	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
)

// EventsRelay is a loopback front door to the hub's POST /events (CW-20261020-0032).
// Services on a worker host publish events to http://127.0.0.1:<port>/events with
// no credential of their own; the relay forwards each request, byte for byte, to
// <hub>/events with the worker's own device credential, and hands the hub's
// answer back unchanged (status, Retry-After and body), so a client's retry
// rules apply as if it talked to the hub.
//
// DEV risk, recorded on purpose: any local process on the host can publish as
// any registered app with the host's credential. The release fix is a per-device
// app allowlist stored with the grant (CW-20261020-0031). A web page is not a
// local process the DEV decision accepted: the relay refuses a request with an
// Origin header, a non-loopback Host (DNS rebinding) or a Content-Type other
// than application/json (so no preflight-free cross-origin POST). It binds to a
// loopback address only, never logs a token or a body, applies the contract's
// size limits itself, and refuses to forward when the worker has no credential.
type EventsRelay struct {
	// Listen is the loopback address to bind, such as 127.0.0.1:7845.
	Listen string
	// Hub is the hub's base URL (https, or loopback http for a local hub).
	Hub string
	// TokenFile is the worker's device credential, re-read for every request
	// because the worker renews it in place.
	TokenFile string
	// Client overrides the HTTP client (tests).
	Client *http.Client
	// Logf receives one-line diagnostics. Never a token or a body.
	Logf func(format string, args ...any)

	inflight atomic.Int32

	// One client, and so one connection pool, for the life of the relay.
	clientOnce sync.Once
	hubClient  *http.Client
	// life is canceled when Serve is asked to stop; it bounds every outbound
	// call so none outlives the relay. Nil (a bare Handler) means no bound.
	lifeMu sync.Mutex
	life   context.Context
}

const (
	relayMaxInflight = 32
	relayHubTimeout  = 30 * time.Second
	relayRetryAfter  = "5"
)

// ErrEventsRelayConfig is returned for a relay that cannot be started safely.
var ErrEventsRelayConfig = errors.New("events relay: listen address must be loopback and hub must be https (or loopback http)")

func (r *EventsRelay) validate() error {
	host, _, err := net.SplitHostPort(r.Listen)
	if err != nil {
		return ErrEventsRelayConfig
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return ErrEventsRelayConfig
	}
	u, err := url.Parse(r.Hub)
	if err != nil || u.Host == "" || u.User != nil {
		return ErrEventsRelayConfig
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" {
			return nil
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return ErrEventsRelayConfig
}

func (r *EventsRelay) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *EventsRelay) client() *http.Client {
	r.clientOnce.Do(func() {
		if r.Client != nil {
			c := *r.Client
			c.CheckRedirect = noRedirect
			r.hubClient = &c
			return
		}
		r.hubClient = &http.Client{Timeout: relayHubTimeout, CheckRedirect: noRedirect,
			Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: relayHubTimeout, TLSHandshakeTimeout: 15 * time.Second,
				IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 4}}
	})
	return r.hubClient
}

func (r *EventsRelay) lifeCtx() context.Context {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()
	return r.life
}

// browserSafe refuses what a web page could send to a loopback port. A page can
// make a "simple" cross-origin POST (text/plain, no preflight) and can reach
// 127.0.0.1 through a rebound DNS name, so the relay requires a JSON content
// type (which forces a preflight the relay never answers), no Origin header
// (non-browser publishers send none) and a loopback Host.
func browserSafe(req *http.Request) (int, string, string) {
	if req.Header.Get("Origin") != "" {
		return http.StatusForbidden, "origin_forbidden", "cross-origin requests are not accepted"
	}
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" {
		return http.StatusForbidden, "host_forbidden", "request host is not loopback"
	}
	mt, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if mt != "application/json" {
		return http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"
	}
	return 0, "", ""
}

// Serve binds Listen and relays until ctx is canceled. ready, if set, receives
// the bound address once the listener is up.
func (r *EventsRelay) Serve(ctx context.Context, ready func(net.Addr)) error {
	if err := r.validate(); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", r.Listen)
	if err != nil {
		return fmt.Errorf("events relay: %w", err)
	}
	srv := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
	// Outbound hub calls hang off ctx, so canceling it aborts a request that is
	// blocked on the hub instead of letting Shutdown wait out its 5s.
	r.lifeMu.Lock()
	r.life = ctx
	r.lifeMu.Unlock()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		r.client().CloseIdleConnections()
	}()
	if ready != nil {
		ready(ln.Addr())
	}
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if err != nil {
		// Serve failed on its own: stop the shutdown goroutine and join it.
		// Without this a failed Serve would leave it waiting on ctx.
		_ = srv.Close()
		r.client().CloseIdleConnections()
		return err
	}
	// ErrServerClosed returns as soon as Shutdown begins; wait for it to finish
	// (in-flight calls drained, idle connections closed) before returning.
	<-stopped
	return nil
}

// Handler is the relay's HTTP surface: POST /events and GET /health.
func (r *EventsRelay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/events", r.handleEvents)
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			relayError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", "")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	return mux
}

func relayError(w http.ResponseWriter, status int, code, msg, retryAfter string) {
	w.Header().Set("Content-Type", "application/json")
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":{"code":`+strconv.Quote(code)+`,"message":`+strconv.Quote(msg)+`}}`)
}

func (r *EventsRelay) handleEvents(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		relayError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", "")
		return
	}
	if status, code, msg := browserSafe(req); status != 0 {
		relayError(w, status, code, msg, "")
		return
	}
	if r.inflight.Add(1) > relayMaxInflight {
		r.inflight.Add(-1)
		relayError(w, http.StatusTooManyRequests, "relay_busy", "too many requests in flight", "1")
		return
	}
	defer r.inflight.Add(-1)

	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, ingest.MaxRequestBytes+1))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			relayError(w, http.StatusRequestEntityTooLarge, "request_too_large", ingest.ErrRequestTooLarge.Error(), "")
			return
		}
		relayError(w, http.StatusBadRequest, "invalid_request", "read request body", "")
		return
	}
	items, err := ingest.ParseRequest(body)
	switch {
	case errors.Is(err, ingest.ErrRequestTooLarge), errors.Is(err, ingest.ErrBatchTooLarge):
		relayError(w, http.StatusRequestEntityTooLarge, "request_too_large", err.Error(), "")
		return
	case err != nil:
		relayError(w, http.StatusBadRequest, "invalid_request", err.Error(), "")
		return
	}
	token, err := identity.ReadTokenFile(r.TokenFile)
	if err != nil {
		// The credential is missing or unreadable: the worker is not enrolled
		// (yet). Retryable; say nothing about why beyond that.
		r.logf("events-relay: no usable device credential; refusing to forward %d event(s)", len(items))
		relayError(w, http.StatusServiceUnavailable, "relay_no_credential", "worker has no usable hub credential", relayRetryAfter)
		return
	}
	// The hub call ends when the caller goes away or the relay is stopped.
	callCtx, stopCall := context.WithCancel(req.Context())
	defer stopCall()
	if life := r.lifeCtx(); life != nil {
		defer context.AfterFunc(life, stopCall)()
	}
	out, err := http.NewRequestWithContext(callCtx, http.MethodPost, strings.TrimRight(r.Hub, "/")+"/events", bytes.NewReader(body))
	if err != nil {
		relayError(w, http.StatusInternalServerError, "relay_error", "cannot build hub request", "")
		return
	}
	out.Header.Set("Authorization", "Bearer "+token)
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set(environment.ProtocolHeader, "1")
	res, err := r.client().Do(out)
	if err != nil {
		r.logf("events-relay: hub unreachable for %d event(s)", len(items))
		relayError(w, http.StatusBadGateway, "relay_hub_unreachable", "hub unreachable", relayRetryAfter)
		return
	}
	defer func() { _ = res.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(res.Body, ingest.MaxRequestBytes))
	if err != nil {
		relayError(w, http.StatusBadGateway, "relay_hub_unreachable", "hub answer unreadable", relayRetryAfter)
		return
	}
	r.logf("events-relay: forwarded %d event(s), hub status %d", len(items), res.StatusCode)
	for _, h := range []string{"Content-Type", "Retry-After"} {
		if v := res.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(answer)
}
