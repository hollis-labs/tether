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

	mcpauth "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/auth"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/runtoken"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// HandlerConfig is daemon composition, never caller-controlled configuration.
type HandlerConfig struct {
	// Backend modes are set only by NewBackend, never request selectors.
	nativeOnly   bool
	remoteOnly   bool
	TeamsEnabled bool
	Teams        api.TeamOps
	Publisher    events.Publisher
	ListenAddr   string
	IdentityMode identity.Mode
	Verifier     identity.Verifier
	Resolver     CallerResolver
	Service      *app.Service
	NativeClient func(string) *client.Client
	// NativeClientSource, when set, is used instead of NativeClient: the client asks fn for the bearer on every
	// request, so a view whose issued token is refreshed in place keeps calling the daemon with the current one.
	NativeClientSource  func(fn func() string) *client.Client
	NewRuntime          func(context.Context, *config.Catalog) (*mcpadapter.SharedUpstreams, error)
	SessionTimeout      time.Duration
	RecheckInterval     time.Duration
	MaxViews            int
	MaxPrincipalViews   int
	VerificationTimeout time.Duration
}

type admission struct {
	run         runtoken.Claims
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
	view            *mcpadapter.GatewayView
	localConnection bool
	// Retained only in memory for stream revocation checks; never exported. Guarded by Handler.mu: an issued
	// agent token is refreshed in place, so the view follows the latest verified token (see adopt).
	token                  string
	tokenExp               time.Time
	runExp                 time.Time
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
	cfg              HandlerConfig
	ctx              context.Context
	cancel           context.CancelFunc
	admissionCtx     context.Context
	cancelAdmissions context.CancelFunc
	done             chan struct{}
	authority        authorityPolicy
	sdk              http.Handler
	mu               sync.Mutex
	closed           bool
	views            map[*transportView]struct{}
	sessions         map[string]*transportView
	runtimeMu        sync.Mutex
	runtime          *mcpadapter.SharedUpstreams
	monitorCancel    context.CancelFunc
	monitorCtx       context.Context
	calls            sync.WaitGroup
	requests         sync.WaitGroup
}

// admittedProvider exposes only identity that the transport has already verified.
// Admission, policy binding and revalidation stay owned by this transport.
type admittedProvider struct{}

var _ mcpauth.Provider = admittedProvider{}

func (admittedProvider) Verifier() auth.TokenVerifier {
	return func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		a, ok := ctx.Value(admissionKey{}).(admission)
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		token := &auth.TokenInfo{UserID: a.fingerprint, Scopes: a.caller.Principal.Scopes}
		if a.caller.Principal.ExpiresAt != nil {
			token.Expiration = *a.caller.Principal.ExpiresAt
		}
		return token, nil
	}
}

func (admittedProvider) Options() *auth.RequireBearerTokenOptions {
	return &auth.RequireBearerTokenOptions{AllowMissingExpiration: true}
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
	if cfg.Resolver.Catalog == nil || (!cfg.nativeOnly && cfg.NewRuntime == nil) {
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
	admissionCtx, cancelAdmissions := context.WithCancel(ctx)
	h := &Handler{admissionCtx: admissionCtx, cancelAdmissions: cancelAdmissions, monitorCtx: monitorCtx, monitorCancel: monitorCancel, cfg: cfg, ctx: ctx, cancel: func() { cancelAdmissions(); cancel() }, done: make(chan struct{}), authority: policy, views: map[*transportView]struct{}{}, sessions: map[string]*transportView{}}
	sdk := mcpsdk.NewStreamableHTTPHandler(func(r *http.Request) *mcpsdk.Server {
		if view, _ := r.Context().Value(preparedViewKey{}).(*transportView); view != nil {
			return view.view.Server
		}
		return nil
	}, &mcpsdk.StreamableHTTPOptions{
		SessionTimeout: cfg.SessionTimeout,
		// The SDK's own DNS-rebinding guard refuses a loopback connection whose Host is not loopback, which is exactly a
		// request relayed to the remote listener by a reverse proxy (Host = the public name). It is replaced, not dropped
		// (architect ruling tether_mcp_sdk_localhost_guard_disabled_2026_10_16): a local request is checked by
		// h.authority below, a remote one only gets its remote provenance from daemon's remoteMCP.serveMCP after the same
		// remoteAuthority Host/Origin check the listener ran. Tests that replace the SDK guard:
		// TestLocalAuthorityReplacesSDKLocalhostGuard and TestCrossOriginBrowserRequestIsRefused (here), TestRemoteMCPMountRepeatsTheListenerAuthority (daemon).
		// Cross-origin protection stays on (http.NewCrossOriginProtection wraps the SDK handler below).
		DisableLocalhostProtection: true,
	})
	h.sdk = mcpauth.HTTPMiddleware(admittedProvider{})(http.NewCrossOriginProtection().Handler(sdk))
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
	if h.cfg.remoteOnly {
		if !identity.IsBoundAgentToken(p) {
			return admission{}, errRemoteCaller
		}
		if h.cfg.Service == nil || h.cfg.Service.Store == nil {
			return admission{}, errIdentityUnavailable
		}
		row, err := h.cfg.Service.Store.GetSession(p.SessionID)
		if errors.Is(err, store.ErrSessionNotFound) {
			return admission{}, identity.ErrInvalidToken
		}
		if err != nil {
			return admission{}, errIdentityUnavailable
		}
		switch row.State {
		case "completed", "failed", "killed":
			return admission{}, identity.ErrInvalidToken
		}
	}
	var run runtoken.Claims
	if s.runToken != "" {
		var runErr error
		run, runErr = h.cfg.Service.VerifyRunToken(s.runToken, p)
		if runErr != nil {
			return admission{}, identity.ErrInvalidToken
		}
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
	a := admission{run: run, caller: caller, options: opts, catalog: cat}
	if h.cfg.nativeOnly {
		if _, err := selectedUpstreamOrigins(a); err != nil {
			return admission{}, err
		}
	}
	key, err := viewFingerprint(caller, token, opts)
	if s.runToken != "" {
		key += ":run:" + identity.HashToken(s.runToken)
	}
	a.fingerprint = key
	return a, err
}

// maxAuthorizationHeader bounds the Authorization header. Stored credentials are short, but an issued
// agent JWT (iss, sub, aud, scope, exp, iat, jti, kind, sid, par plus an EdDSA signature) is about 470
// characters, so the old 256 cap refused every issued token before it was verified.
const maxAuthorizationHeader = 4096

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rl := &refusalLog{ResponseWriter: w}
	w = rl
	defer func() {
		if rl.status >= 400 {
			slog.Info("daemon MCP refused", "status", rl.status, "method", r.Method, "remote", identity.IsRemote(r.Context()), "detail", rl.detail()) //nolint:gosec // G706 false positive: structured slog attrs are quoted/encoded by the handler; detail is the refusal body Tether wrote
		}
	}()
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
	// A request with server-owned remote provenance came through the remote listener, whose exact Host and Origin
	// allow-lists already ran; the local authority (unix/loopback names) does not apply to it.
	if !identity.IsRemote(r.Context()) && !h.authority.validate(r) {
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
	if len(values) != 1 || len(values[0]) > maxAuthorizationHeader {
		transportError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		transportError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.runToken, err = runtoken.FromHeader(r.Header)
	if err != nil {
		transportError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	a, err := h.admit(r.Context(), parts[1], s)
	if err != nil {
		admissionError(w, err)
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
		if v.fingerprint != a.fingerprint || v.localConnection != identity.LocalConnection(r.Context()) {
			transportError(w, http.StatusForbidden, "mcp_session_policy_mismatch")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), preparedViewKey{}, v))
		h.adopt(v, parts[1], a.caller.Principal.ExpiresAt)
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
		stage := prepareStageOf(err)
		// The stage names the failing step and is safe to return; the cause stays in the daemon log, bounded.
		slog.Warn("daemon MCP view preparation failed", "stage", stage, "principal_kind", a.caller.Principal.Kind, "error", boundedError(err))
		transportError(w, http.StatusServiceUnavailable, "mcp_view_unavailable_"+stage)
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

// viewToken is the latest verified token of v.
func (h *Handler) viewToken(v *transportView) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return v.token
}

// adopt makes token the view's current credential after the request that carried it verified and matched the
// view's fingerprint. Only a token that expires later replaces the current one, so a late, older request cannot
// move the view back to a credential about to lapse.
func (h *Handler) adopt(v *transportView, token string, exp *time.Time) {
	if exp == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Equal counts as newer: expiry has one-second resolution, so a refresh minted in the same second is current.
	if token != v.token && !exp.Before(v.tokenExp) {
		v.token, v.tokenExp = token, *exp
	}
}

// Stages of view preparation, named in the 503 so a refusal can be read without the daemon log.
const (
	stageCapacity   = "view_capacity"
	stageRuntime    = "runtime_create"
	stageOriginPick = "origin_select"
	stageOriginRun  = "origin_start"
	stageAdapter    = "adapter_build"
	stageGateway    = "gateway_view"
	stageStopping   = "stopping"
)

// prepareError tags a preparation failure with the stage that produced it.
type prepareError struct {
	stage string
	err   error
}

func (e *prepareError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *prepareError) Unwrap() error { return e.err }

func stageErr(stage string, err error) error { return &prepareError{stage: stage, err: err} }

func prepareStageOf(err error) string {
	var pe *prepareError
	if errors.As(err, &pe) {
		return pe.stage
	}
	return "unknown"
}

func boundedError(err error) string {
	text := err.Error()
	if len(text) > 500 {
		text = text[:500] + "..."
	}
	return text
}

func (h *Handler) prepare(ctx context.Context, a admission, token string, s selectors) (*transportView, error) {
	v := &transportView{token: token, selectors: s, fingerprint: a.fingerprint, principalKey: a.caller.Principal.Kind + ":" + a.caller.Principal.ID, operator: identity.IsOperatorCredential(a.caller.Principal), lastUsed: time.Now()}
	if a.run.RunID != "" {
		v.runExp = time.Unix(a.run.Expires, 0)
	}
	if a.caller.Principal.ExpiresAt != nil {
		v.tokenExp = *a.caller.Principal.ExpiresAt
	}
	v.localConnection = identity.LocalConnection(ctx)
	v.callCtx, v.cancelCalls = context.WithCancel(h.ctx)     //nolint:gosec // G118: v.cancelCalls is called when the view is closed or refused
	v.streamCtx, v.cancelStreams = context.WithCancel(h.ctx) //nolint:gosec // G118: v.cancelStreams is called when the view is closed or refused
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
		return nil, stageErr(stageCapacity, fmt.Errorf("MCP view capacity unavailable"))
	}
	h.views[v] = struct{}{}
	h.mu.Unlock()
	success := false
	defer func() {
		if !success {
			h.remove(v)
		}
	}()
	var pool *mcpadapter.SharedUpstreams
	if !h.cfg.nativeOnly {
		h.runtimeMu.Lock()
		if h.runtime == nil {
			var err error
			h.runtime, err = h.cfg.NewRuntime(h.ctx, a.catalog)
			if err != nil {
				h.runtimeMu.Unlock()
				return nil, stageErr(stageRuntime, err)
			}
		}
		pool = h.runtime
		if pool == nil {
			h.runtimeMu.Unlock()
			return nil, stageErr(stageRuntime, fmt.Errorf("MCP runtime factory returned no pool"))
		}
		h.runtimeMu.Unlock()
		origins, err := selectedUpstreamOrigins(a)
		if err != nil {
			return nil, stageErr(stageOriginPick, err)
		}
		if len(origins) > 0 {
			err := pool.StartOrigins(h.ctx, origins)
			var collision *mcpgateway.CollisionError
			if err != nil && !errors.As(err, &collision) {
				return nil, stageErr(stageOriginRun, err)
			}
			if collision != nil {
				slog.Warn("daemon MCP name collision; healthy origins remain available", "collisions", collision.Collisions)
			}
		}
	}
	var dc *client.Client
	switch {
	case h.cfg.NativeClientSource != nil:
		dc = h.cfg.NativeClientSource(func() string { return h.viewToken(v) })
	case h.cfg.NativeClient != nil:
		dc = h.cfg.NativeClient(token)
	}
	adapter, err := mcpadapter.NewVerifiedAdapterWith(ctx, h.cfg.Service, dc, h.cfg.Verifier)
	if err != nil {
		return nil, stageErr(stageAdapter, err)
	}
	adapter.ExtractRefs = a.caller.Policy.ExtractRefs
	// The session's editable_agents allowlist rides in its saved policy: a
	// session on the hub relay keeps the restriction the ssh-launched
	// `tether mcp --editable-agent` flags carried. Empty = unrestricted, as before.
	adapter.SetEditableAgents(a.caller.Policy.EditableAgents)
	if h.cfg.TeamsEnabled && api.HasTeamOps(h.cfg.Teams) {
		adapter.SetTeams(h.cfg.Teams)
	}
	p := a.caller.Principal
	if p.Verified && (p.Kind == "session" || identity.IsBoundAgentToken(p)) &&
		p.SessionID != "" && p.SessionID == a.caller.Policy.SessionID &&
		h.cfg.Service != nil && h.cfg.Service.Store != nil {
		adapter.SetRefAttacher(hubRefAttacher{sessionID: p.SessionID, store: h.cfg.Service.Store})
	}
	a.options.LiveServers = func() ([]string, error) {
		cat, err := h.cfg.Resolver.Catalog(h.ctx)
		if err != nil {
			return nil, err
		}
		if cat == nil {
			return nil, errCatalogUnavailable
		}
		ids, _ := cat.NarrowMCPGrant(a.caller.Policy.Servers)
		return ids, nil
	}
	a.options.Publisher = h.cfg.Publisher
	if h.cfg.Service != nil && h.cfg.Service.Store != nil {
		a.options.ProxyStore = h.cfg.Service.Store
	}
	viewCtx := identity.WithPrincipal(h.ctx, a.caller.Principal)
	if a.run.RunID != "" {
		viewCtx = runtoken.WithClaims(viewCtx, a.run)
	}
	var view *mcpadapter.GatewayView
	if h.cfg.nativeOnly {
		view, err = adapter.NewNativeView(viewCtx, a.options)
	} else {
		view, err = pool.NewGatewayView(viewCtx, adapter, a.options)
	}
	if err != nil {
		return nil, stageErr(stageGateway, err)
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
			current, err := h.admit(ctx, h.viewToken(v), s)
			if err != nil || current.fingerprint != v.fingerprint {
				return nil, fmt.Errorf("MCP credential or policy no longer valid")
			}
			ctx = identity.WithPrincipal(ctx, current.caller.Principal)
			if current.run.RunID != "" {
				ctx = runtoken.WithClaims(ctx, current.run)
			}
			if h.cfg.remoteOnly {
				ctx = identity.WithRemoteContext(identity.WithConnectionContext(ctx, nil))
			}
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
		return nil, stageErr(stageStopping, fmt.Errorf("MCP stopping"))
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

// expireCredential uses the latest verified expiry even when authority stores
// are unavailable. Check and cancellation share adopt's lock, so a refresh
// already accepted by the view cannot be replaced by an older expiry snapshot.
func (h *Handler) expireCredential(v *transportView) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if (v.tokenExp.IsZero() || time.Now().Before(v.tokenExp)) && (v.runExp.IsZero() || time.Now().Before(v.runExp)) {
		return false
	}
	v.cancelCalls()
	v.cancelStreams()
	return true
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
				if h.expireCredential(v) {
					h.remove(v)
					continue
				}
				ctx, cancel := context.WithTimeout(h.monitorCtx, h.cfg.VerificationTimeout)
				a, err := h.admit(ctx, h.viewToken(v), v.selectors)
				cancel()
				if h.expireCredential(v) {
					h.remove(v)
					continue
				}
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

// refusalLog records the status and the first bytes of an error body so every non-2xx the MCP route answers leaves one log
// line with its reason, whether transportError or the SDK wrote it. The body of an error carries a code or a short message,
// never a token. Flush and Unwrap keep streaming responses working.
type refusalLog struct {
	http.ResponseWriter
	status int
	body   []byte
}

func (w *refusalLog) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *refusalLog) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 && len(w.body) < 200 {
		w.body = append(w.body, data[:min(len(data), 200-len(w.body))]...)
	}
	return w.ResponseWriter.Write(data)
}

func (w *refusalLog) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w *refusalLog) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *refusalLog) detail() string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return ' '
		}
		return r
	}, strings.TrimSpace(string(w.body)))
}

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
		h.cancelAdmissions()
		h.mu.Lock()
		for v := range h.views {
			v.cancelCalls()
			v.cancelStreams()
		}
		h.mu.Unlock()
	}
}

// ToolCallRecorderStats exposes drops/failures without changing MCP admission.
func (h *Handler) ToolCallRecorderStats() (uint64, uint64) {
	if h == nil {
		return 0, 0
	}
	if recorder, ok := h.cfg.Publisher.(interface{ Stats() (uint64, uint64) }); ok {
		return recorder.Stats()
	}
	return 0, 0
}
