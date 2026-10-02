package mcptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// HandlerConfig is daemon composition, never caller-controlled configuration.
type HandlerConfig struct {
	ListenAddr          string
	IdentityMode        identity.Mode
	Verifier            identity.Verifier
	Resolver            CallerResolver
	Service             *app.Service
	NativeClient        func(string) *client.Client
	NewRuntime          func(context.Context, *config.Catalog) (*mcpadapter.SharedUpstreams, error)
	SessionTimeout      time.Duration
	RecheckInterval     time.Duration
	MaxViews            int
	MaxPrincipalViews   int
	VerificationTimeout time.Duration
}

type admission struct {
	caller      Caller
	options     mcpadapter.ProxyOptions
	catalog     *config.Catalog
	fingerprint string
}

var errIdentityUnavailable = errors.New("MCP identity verifier unavailable")
var errCatalogUnavailable = errors.New("MCP catalog unavailable")

type admissionKey struct{}
type preparedViewKey struct{}

type transportView struct {
	view *mcpadapter.GatewayView
	// Retained only in memory for stream revocation checks; never exported.
	token                  string
	selectors              selectors
	fingerprint, sessionID string
	ready                  bool
	principalKey           string
	operator               bool
	callCtx                context.Context
	cancelCalls            context.CancelFunc
	streamCtx              context.Context
	cancelStreams          context.CancelFunc
	lastUsed               time.Time
	active                 int
}

// Handler owns SDK sessions/views and one lazily constructed upstream runtime.
// Close must run before the daemon closes its store or drains HTTP streams.
type Handler struct {
	cfg           HandlerConfig
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	authority     authorityPolicy
	sdk           http.Handler
	mu            sync.Mutex
	closed        bool
	views         map[*transportView]struct{}
	sessions      map[string]*transportView
	runtimeMu     sync.Mutex
	runtime       *mcpadapter.SharedUpstreams
	monitorCancel context.CancelFunc
	monitorCtx    context.Context
	calls         sync.WaitGroup
	requests      sync.WaitGroup
}

func NewHandler(ctx context.Context, cfg HandlerConfig) (*Handler, error) {
	if err := identity.ValidateBind(cfg.ListenAddr, cfg.IdentityMode); err != nil {
		return nil, err
	}
	if cfg.IdentityMode == identity.Enforce && cfg.Verifier == nil {
		return nil, fmt.Errorf("MCP enforce requires an identity verifier")
	}
	if cfg.IdentityMode == identity.Enforce {
		probe, err := identity.NewToken()
		if err != nil {
			return nil, fmt.Errorf("MCP identity readiness unavailable")
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = cfg.Verifier.Verify(probeCtx, probe)
		cancel()
		if err != nil && !errors.Is(err, identity.ErrInvalidToken) {
			return nil, fmt.Errorf("MCP enforce requires a working identity verifier")
		}
	}
	policy, err := newAuthorityPolicy(cfg.ListenAddr)
	if err != nil {
		return nil, err
	}
	if cfg.Resolver.Catalog == nil || cfg.NewRuntime == nil {
		return nil, fmt.Errorf("MCP catalog and runtime factories are required")
	}
	if cfg.SessionTimeout <= 0 {
		cfg.SessionTimeout = 15 * time.Minute
	}
	if cfg.RecheckInterval <= 0 {
		cfg.RecheckInterval = time.Second
	}
	if cfg.MaxViews <= 0 {
		cfg.MaxViews = 128
	}
	if cfg.MaxPrincipalViews <= 0 {
		cfg.MaxPrincipalViews = 16
	}
	if cfg.VerificationTimeout <= 0 {
		cfg.VerificationTimeout = 2 * time.Second
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	monitorCtx, monitorCancel := context.WithCancel(ctx)
	h := &Handler{monitorCtx: monitorCtx, monitorCancel: monitorCancel, cfg: cfg, ctx: ctx, cancel: cancel, done: make(chan struct{}), authority: policy, views: map[*transportView]struct{}{}, sessions: map[string]*transportView{}}
	sdk := mcpsdk.NewStreamableHTTPHandler(func(r *http.Request) *mcpsdk.Server {
		if view, _ := r.Context().Value(preparedViewKey{}).(*transportView); view != nil {
			return view.view.Server
		}
		return nil
	}, &mcpsdk.StreamableHTTPOptions{SessionTimeout: cfg.SessionTimeout})
	h.sdk = auth.RequireBearerToken(func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		a, ok := ctx.Value(admissionKey{}).(admission)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		token := &auth.TokenInfo{UserID: a.fingerprint, Scopes: a.caller.Principal.Scopes}
		if a.caller.Principal.ExpiresAt != nil {
			token.Expiration = *a.caller.Principal.ExpiresAt
		}
		return token, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(sdk)
	go h.monitor()
	return h, nil
}

func (h *Handler) admit(ctx context.Context, token string, s selectors) (admission, error) {
	p, err := h.cfg.Verifier.Verify(ctx, token)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidToken) {
			return admission{}, identity.ErrInvalidToken
		}
		return admission{}, errIdentityUnavailable
	}
	ctx = identity.WithPrincipal(ctx, p)
	cat, err := h.cfg.Resolver.Catalog(ctx)
	if err != nil || cat == nil {
		return admission{}, errCatalogUnavailable
	}
	r := h.cfg.Resolver
	r.Catalog = func(context.Context) (*config.Catalog, error) { return cat, nil }
	caller, err := r.Resolve(ctx)
	if err != nil {
		return admission{}, err
	}
	opts, err := ViewOptions(caller, cat.Global.MCP, s.profiles, s.modes)
	if err != nil {
		return admission{}, err
	}
	key, err := viewFingerprint(caller, token, opts)
	return admission{caller: caller, options: opts, catalog: cat, fingerprint: key}, err
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		transportError(w, http.StatusServiceUnavailable, "mcp_stopping")
		return
	}
	if r.Method == http.MethodPost {
		h.requests.Add(1)
	}
	h.mu.Unlock()
	if r.Method == http.MethodPost {
		defer h.requests.Done()
	}

	if h.ctx.Err() != nil {
		transportError(w, http.StatusServiceUnavailable, "mcp_stopping")
		return
	}
	if !h.authority.validate(r) {
		transportError(w, http.StatusForbidden, "mcp_origin_refused")
		return
	}
	if h.cfg.IdentityMode == identity.Off || h.cfg.IdentityMode == "" || h.cfg.Verifier == nil {
		transportError(w, http.StatusServiceUnavailable, "identity_unavailable")
		return
	}
	s, err := requestSelectors(r)
	if err != nil {
		transportError(w, http.StatusBadRequest, "invalid_mcp_selectors")
		return
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > 256 {
		transportError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		transportError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	a, err := h.admit(r.Context(), parts[1], s)
	if err != nil {
		status, code := http.StatusForbidden, "mcp_policy_refused"
		if errors.Is(err, identity.ErrInvalidToken) {
			status, code = http.StatusUnauthorized, "unauthorized"
		} else if errors.Is(err, errIdentityUnavailable) {
			status, code = http.StatusServiceUnavailable, "identity_unavailable"
		} else if errors.Is(err, errCatalogUnavailable) || errors.Is(err, errSessionUnavailable) {
			status, code = http.StatusServiceUnavailable, "mcp_catalog_unavailable"
		}
		transportError(w, status, code)
		return
	}
	r = r.WithContext(identity.WithPrincipal(r.Context(), a.caller.Principal))
	r = r.WithContext(context.WithValue(r.Context(), admissionKey{}, a))
	ids := r.Header.Values("Mcp-Session-Id")
	if len(ids) > 1 {
		transportError(w, http.StatusBadRequest, "invalid_mcp_session")
		return
	}
	id := r.Header.Get("Mcp-Session-Id")
	if id != "" {
		h.mu.Lock()
		v := h.sessions[id]
		closed := h.closed
		h.mu.Unlock()
		if closed {
			transportError(w, http.StatusServiceUnavailable, "mcp_stopping")
			return
		}
		if v == nil {
			transportError(w, http.StatusNotFound, "mcp_session_not_found")
			return
		}
		if v.fingerprint != a.fingerprint {
			transportError(w, http.StatusForbidden, "mcp_session_policy_mismatch")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), preparedViewKey{}, v))
		h.mu.Lock()
		v.lastUsed = time.Now()
		h.mu.Unlock()
		if r.Method == http.MethodGet {
			streamCtx, cancel := context.WithCancel(r.Context())
			stop := context.AfterFunc(v.streamCtx, cancel)
			defer func() { stop(); cancel() }()
			r = r.WithContext(streamCtx)
		}
		h.sdk.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodPost {
		transportError(w, http.StatusBadRequest, "mcp_session_required")
		return
	}
	if !initializeRequest(w, r) {
		return
	}
	v, err := h.prepare(r.Context(), a, parts[1], s)
	if err != nil {
		slog.Warn("daemon MCP view preparation failed", "reason", "runtime or credential policy unavailable")
		transportError(w, http.StatusServiceUnavailable, "mcp_view_unavailable")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), preparedViewKey{}, v))
	capture := &sessionResponse{ResponseWriter: w, record: func() {
		if id := w.Header().Get("Mcp-Session-Id"); id != "" {
			h.mu.Lock()
			_, present := h.views[v]
			if !h.closed && present {
				v.sessionID = id
				v.ready = true
				h.sessions[id] = v
			}
			h.mu.Unlock()
		}
	}}
	h.sdk.ServeHTTP(capture, r)
	h.mu.Lock()
	v.ready = true
	missing := v.sessionID == ""
	h.mu.Unlock()
	if missing {
		h.remove(v)
	}
}

func initializeRequest(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, mcpsdk.DefaultMaxRequestBodyBytes)
	data, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		transportError(w, http.StatusRequestEntityTooLarge, "invalid_mcp_body")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	var call struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(data, &call) != nil || call.Method != "initialize" {
		transportError(w, http.StatusBadRequest, "mcp_initialize_required")
		return false
	}
	return true
}

func (h *Handler) prepare(ctx context.Context, a admission, token string, s selectors) (*transportView, error) {
	v := &transportView{token: token, selectors: s, fingerprint: a.fingerprint, principalKey: a.caller.Principal.Kind + ":" + a.caller.Principal.ID, operator: a.caller.Principal.Kind == "operator" && a.caller.Principal.ID == identity.OperatorID, lastUsed: time.Now()}
	v.callCtx, v.cancelCalls = context.WithCancel(h.ctx)
	v.streamCtx, v.cancelStreams = context.WithCancel(h.ctx)
	h.mu.Lock()
	count, nonoperator := 0, 0
	for existing := range h.views {
		if existing.principalKey == v.principalKey {
			count++
		}
		if !existing.operator {
			nonoperator++
		}
	}
	reserve := h.cfg.MaxPrincipalViews
	if reserve >= h.cfg.MaxViews {
		reserve = h.cfg.MaxViews / 4
	}
	if h.closed || len(h.views) >= h.cfg.MaxViews || count >= h.cfg.MaxPrincipalViews || (!v.operator && nonoperator >= h.cfg.MaxViews-reserve) {
		v.cancelCalls()
		v.cancelStreams()
		h.mu.Unlock()
		return nil, fmt.Errorf("MCP view capacity unavailable")
	}
	h.views[v] = struct{}{}
	h.mu.Unlock()
	success := false
	defer func() {
		if !success {
			h.remove(v)
		}
	}()
	h.runtimeMu.Lock()
	if h.runtime == nil {
		var err error
		h.runtime, err = h.cfg.NewRuntime(h.ctx, a.catalog)
		if err != nil {
			h.runtimeMu.Unlock()
			return nil, err
		}
	}
	pool := h.runtime
	if pool == nil {
		h.runtimeMu.Unlock()
		return nil, fmt.Errorf("MCP runtime factory returned no pool")
	}
	h.runtimeMu.Unlock()
	origins, err := selectedUpstreamOrigins(a)
	if err != nil {
		return nil, err
	}
	if len(origins) > 0 {
		err := pool.StartOrigins(h.ctx, origins)
		var collision *mcpgateway.CollisionError
		if err != nil && !errors.As(err, &collision) {
			return nil, err
		}
		if collision != nil {
			slog.Warn("daemon MCP name collision; healthy origins remain available", "collisions", collision.Collisions)
		}
	}
	var dc *client.Client
	if h.cfg.NativeClient != nil {
		dc = h.cfg.NativeClient(token)
	}
	adapter, err := mcpadapter.NewVerifiedAdapter(ctx, h.cfg.Service, dc)
	if err != nil {
		return nil, err
	}
	view, err := pool.NewGatewayView(identity.WithPrincipal(h.ctx, a.caller.Principal), adapter, a.options)
	if err != nil {
		return nil, err
	}
	view.Server.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			h.mu.Lock()
			if h.closed || v.callCtx.Err() != nil {
				h.mu.Unlock()
				return nil, fmt.Errorf("MCP stopping")
			}
			h.calls.Add(1)
			v.active++
			v.lastUsed = time.Now()
			h.mu.Unlock()
			defer func() { h.mu.Lock(); v.active--; h.mu.Unlock(); h.calls.Done() }()
			current, err := h.admit(ctx, token, s)
			if err != nil || current.fingerprint != v.fingerprint {
				return nil, fmt.Errorf("MCP credential or policy no longer valid")
			}
			ctx = identity.WithPrincipal(ctx, current.caller.Principal)
			callCtx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(v.callCtx, cancel)
			defer func() { stop(); cancel() }()
			return next(callCtx, method, req)
		}
	})
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		view.Close()
		return nil, fmt.Errorf("MCP stopping")
	}
	v.view = view
	h.mu.Unlock()
	success = true
	return v, nil
}

func (h *Handler) remove(v *transportView) {
	h.mu.Lock()
	delete(h.views, v)
	delete(h.sessions, v.sessionID)
	view := v.view
	v.cancelCalls()
	v.cancelStreams()
	h.mu.Unlock()
	if view != nil {
		view.Close()
	}
}

func (h *Handler) monitor() {
	defer close(h.done)
	ticker := time.NewTicker(h.cfg.RecheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.monitorCtx.Done():
			return
		case <-ticker.C:
			h.mu.Lock()
			views := []*transportView{}
			for v := range h.views {
				if v.ready && v.view != nil {
					views = append(views, v)
				}
			}
			h.mu.Unlock()
			for _, v := range views {
				ctx, cancel := context.WithTimeout(h.monitorCtx, h.cfg.VerificationTimeout)
				a, err := h.admit(ctx, v.token, v.selectors)
				cancel()
				alive := false
				for range v.view.Server.Sessions() {
					alive = true
					break
				}
				h.mu.Lock()
				idle := time.Since(v.lastUsed)
				active := v.active > 0
				h.mu.Unlock()
				timeout := h.cfg.SessionTimeout
				if !v.operator && timeout > 2*time.Minute {
					timeout = 2 * time.Minute
				}
				transient := errors.Is(err, errIdentityUnavailable) || errors.Is(err, errCatalogUnavailable) || errors.Is(err, errSessionUnavailable)
				if transient {
					slog.Warn("daemon MCP verification temporarily unavailable; view retained", "reason", "verification unavailable")
				}
				if !alive || (!active && idle > timeout) || (!transient && (err != nil || a.fingerprint != v.fingerprint)) {
					h.remove(v)
				}
			}
		}
	}
}

func (h *Handler) Close() {
	h.mu.Lock()
	if h.ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	h.closed = true
	views := []*transportView{}
	for v := range h.views {
		views = append(views, v)
	}
	h.mu.Unlock()
	h.monitorCancel()
	h.cancel()
	<-h.done
	for _, v := range views {
		h.remove(v)
	}
	h.runtimeMu.Lock()
	if h.runtime != nil {
		h.runtime.Close()
	}
	h.runtimeMu.Unlock()
}

type sessionResponse struct {
	http.ResponseWriter
	record func()
}

func (w *sessionResponse) WriteHeader(status int) { w.record(); w.ResponseWriter.WriteHeader(status) }
func (w *sessionResponse) Write(data []byte) (int, error) {
	w.record()
	return w.ResponseWriter.Write(data)
}
func (w *sessionResponse) Flush() {
	w.record()
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *sessionResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func transportError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
}

// Drain refuses new calls and ends streams, then lets admitted calls finish.
// On timeout cancellation leaves the upstream outcome unknown; no replay.
func (h *Handler) Drain(ctx context.Context) {
	h.mu.Lock()
	h.closed = true
	for v := range h.views {
		if v.active == 0 {
			v.cancelStreams()
		}
	}
	h.mu.Unlock()
	done := make(chan struct{})
	go func() { h.requests.Wait(); h.calls.Wait(); close(done) }()
	select {
	case <-done:
		h.mu.Lock()
		for v := range h.views {
			v.cancelStreams()
		}
		h.mu.Unlock()
	case <-ctx.Done():
		h.mu.Lock()
		for v := range h.views {
			v.cancelCalls()
			v.cancelStreams()
		}
		h.mu.Unlock()
	}
}
