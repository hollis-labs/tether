package mcptransport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/runtoken"
)

const (
	AdmissionPath       = "/gateway/mcp/admit"
	RemoteAdmissionPath = "/gateway/mcp/remote/admit"
	NativePath          = "/gateway/mcp/native"
	RemoteNativePath    = "/gateway/mcp/remote/native"
)

var errRemoteCaller = errors.New("remote MCP requires a bound agent credential")

// AdmissionPrincipal deliberately excludes credential IDs, token claims and
// connection proof. This response describes the presented bearer's authority;
// it cannot establish identity on a subsequent request.
type AdmissionPrincipal struct {
	Verified  bool       `json:"verified"`
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Scopes    []string   `json:"scopes"`
	SessionID string     `json:"session_id,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// AdmissionResponse is the versioned, secret-free contract with the standalone
// gateway. Every operation must obtain it afresh; Fingerprint is not a grant.
type AdmissionResponse struct {
	Run               *runtoken.Claims              `json:"run,omitempty"`
	SchemaVersion     int                           `json:"schema_version"`
	Principal         AdmissionPrincipal            `json:"principal"`
	Policy            mcpgateway.SessionPolicy      `json:"policy"`
	MachineAccess     string                        `json:"machine_access"`
	Profile           mcpgateway.ProfileSelection   `json:"profile"`
	AuthorityProfiles []mcpgateway.ProfileSelection `json:"authority_profiles"`
	Selection         mcpgateway.Selection          `json:"selection"`
	Fingerprint       string                        `json:"fingerprint"`
}

// Backend is only for a co-located gateway over a protected Unix socket. The
// composition root must not mount it on network listeners. ServeHTTP also checks
// accepted-socket proof, so a mount mistake cannot turn it into a network API.
// The remote route pair only narrows authority. It never trusts a caller's
// forwarding headers or an asserted principal, scopes, session or provenance.
type Backend struct{ local, remote *Handler }

func NewBackend(ctx context.Context, cfg HandlerConfig) (*Backend, error) {
	cfg.nativeOnly = true
	cfg.NewRuntime = nil
	cfg.remoteOnly = false
	local, err := NewHandler(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cfg.remoteOnly = true
	remote, err := NewHandler(ctx, cfg)
	if err != nil {
		local.Close()
		return nil, err
	}
	return &Backend{local: local, remote: remote}, nil
}

func (b *Backend) Close() { b.local.Close(); b.remote.Close() }
func (b *Backend) Drain(ctx context.Context) {
	// Refuse both route classes before waiting on either one's admitted work.
	for _, h := range []*Handler{b.local, b.remote} {
		h.mu.Lock()
		h.closed = true
		h.mu.Unlock()
	}
	b.local.Drain(ctx)
	b.remote.Drain(ctx)
}

func (b *Backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !identity.LocalConnection(r.Context()) || identity.IsRemote(r.Context()) {
		transportError(w, http.StatusNotFound, "remote_route_unavailable")
		return
	}
	if !b.local.authority.validate(r) {
		transportError(w, http.StatusForbidden, "mcp_origin_refused")
		return
	}
	h, native := b.local, false
	switch r.URL.Path {
	case AdmissionPath:
	case RemoteAdmissionPath:
		h = b.remote
	case NativePath:
		native = true
	case RemoteNativePath:
		h, native = b.remote, true
	default:
		transportError(w, http.StatusNotFound, "remote_route_unavailable")
		return
	}
	if h.cfg.remoteOnly {
		r = r.WithContext(identity.WithRemoteContext(identity.WithConnectionContext(r.Context(), nil)))
	}
	if native {
		if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete {
			transportError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		// Preserve selectors and SDK headers; only the internal route changes.
		copyURL := *r.URL
		copyURL.Path = "/mcp"
		copyURL.RawPath = ""
		r = r.Clone(r.Context())
		r.URL = &copyURL
		h.ServeHTTP(w, r)
		return
	}
	h.serveAdmission(w, r)
}

func (h *Handler) serveAdmission(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		transportError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	h.mu.Lock()
	closed := h.closed || h.ctx.Err() != nil
	if !closed {
		h.requests.Add(1)
	}
	h.mu.Unlock()
	if closed {
		transportError(w, http.StatusServiceUnavailable, "mcp_stopping")
		return
	}
	defer h.requests.Done()
	if h.cfg.IdentityMode == identity.Off || h.cfg.IdentityMode == "" || h.cfg.Verifier == nil {
		transportError(w, http.StatusServiceUnavailable, "identity_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.VerificationTimeout)
	stop := context.AfterFunc(h.admissionCtx, cancel)
	// Include body parsing in the request lifetime. A slow writer must not keep
	// draining alive, or begin reading authority after shutdown canceled it.
	stopRead := context.AfterFunc(ctx, func() { _ = http.NewResponseController(w).SetReadDeadline(time.Now()) })
	defer func() { stop(); stopRead(); cancel() }()
	s, err := admissionSelectors(w, r)
	if ctx.Err() != nil || h.admissionCtx.Err() != nil {
		transportError(w, http.StatusServiceUnavailable, "mcp_stopping")
		return
	}
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
	a, err := h.admit(ctx, parts[1], s)
	if err != nil {
		admissionError(w, err)
		return
	}
	selection, err := mcpgateway.ResolveMode(a.options.ModeInputs)
	if err != nil {
		admissionError(w, err)
		return
	}
	p := a.caller.Principal
	access := launch.MachineAccessReadOnly
	if a.caller.MachineAccess == launch.MachineAccessReadWrite {
		access = launch.MachineAccessReadWrite
	}
	response := AdmissionResponse{SchemaVersion: 1,
		Principal: AdmissionPrincipal{Verified: p.Verified, ID: p.ID, Kind: p.Kind, Scopes: append([]string{}, p.Scopes...), SessionID: p.SessionID, ExpiresAt: p.ExpiresAt},
		Policy:    a.caller.Policy, MachineAccess: access, Profile: a.options.Profile,
		AuthorityProfiles: a.options.AuthorityProfiles, Selection: selection, Fingerprint: a.fingerprint,
	}
	if a.run.RunID != "" {
		response.Run = &a.run
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// Decode a bounded object without accepting nulls, duplicate keys or a second
// JSON value. Admission has one selector source; native MCP retains its existing
// query/header conflict checks. Neither accepts authority claims.
func admissionSelectors(w http.ResponseWriter, r *http.Request) (selectors, error) {
	var s selectors
	invalid := errors.New("invalid admission selectors")
	if r.URL.RawQuery != "" || len(r.Header.Values("X-Tether-Profile")) != 0 || len(r.Header.Values("X-Tether-Discovery-Mode")) != 0 {
		return s, invalid
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return s, invalid
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return s, invalid
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return s, invalid
		}
		seen[name] = true
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return s, invalid
		}
		var value string
		if string(raw) == "null" || json.Unmarshal(raw, &value) != nil || value == "" {
			return s, invalid
		}
		switch name {
		case "profile":
			s.profiles = append(s.profiles, mcpgateway.Selector{Value: value, Source: "request"})
		case "discovery_mode":
			s.modes = append(s.modes, mcpgateway.Selector{Value: value, Source: "request"})
		default:
			return s, invalid
		}
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') {
		return s, invalid
	}
	if _, err := d.Token(); err != io.EOF {
		return s, invalid
	}
	return s, nil
}

func admissionError(w http.ResponseWriter, err error) {
	status, code := http.StatusForbidden, "mcp_policy_refused"
	switch {
	case errors.Is(err, identity.ErrInvalidToken):
		status, code = http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, errRemoteCaller):
		status, code = http.StatusNotFound, "remote_route_unavailable"
	case errors.Is(err, errIdentityUnavailable):
		status, code = http.StatusServiceUnavailable, "identity_unavailable"
	case errors.Is(err, errCatalogUnavailable), errors.Is(err, errSessionUnavailable):
		status, code = http.StatusServiceUnavailable, "mcp_catalog_unavailable"
	}
	transportError(w, status, code)
}
