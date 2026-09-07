package api

// registry.go — HTTP surface for the v0.6 federation directory service
// (T-v060-01-05). The package-level RegistryService interface is the seam
// the handlers depend on; *registry.Service satisfies it through method
// promotion. Wiring lives in registerRegistryRoutes (called from
// server.go's NewHandler) and only mounts when Server.Registry is non-nil.
//
// Routing strategy. The api package uses stdlib net/http with the older
// "prefix-then-parse" idiom (see broker.go, sessions.go). Registry follows
// suit: a single handler mounted at "/registry/" parses the remainder of
// the path into (kindSegment, urn, action) tuples. We deliberately avoid
// Go 1.22 enhanced patterns to stay consistent with the rest of the
// package.
//
// URL kind segment is plural ("agents", "projects") to match REST
// conventions. The internal registry.Kind type stays singular ("agent",
// "project"). The translation happens here at the API boundary; see
// kindFromSegment.
//
// Error mapping (locked to existing api/errors.go codes):
//
//	ErrInvalidRequest                   → 400 invalid_request
//	ErrNotFound                         → 404 not_found
//	ErrNoCallback                       → 204 No Content (no body)
//	ErrNoResolver                       → 400 invalid_request — see note
//	ErrPayloadInvalid                   → 502 internal_error (upstream callback)
//	ErrPayloadTooLarge                  → 502 internal_error (upstream callback)
//	ErrPathOutsideRoot                  → 502 internal_error (upstream callback)
//	ErrMintExhausted                    → 503 internal_error
//	other                               → 500 internal_error
//
// Design note on ErrNoResolver. The choice between 400 and 422 is
// genuinely ambiguous — the caller's request is well-formed but the
// server's resolver-registration table is empty for the row's scheme.
// We pick 400 here so the (small, hand-coded) error-code catalog
// stays stable; the message ("no resolver registered for scheme cli")
// is precise enough that operators can correct configuration without a
// new status code.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/registry"
)

// RegistryService is the seam the registry HTTP handlers depend on.
// *registry.Service satisfies it through method promotion; tests may
// supply a stub. The interface intentionally mirrors the Service's
// public surface so callers wire one and the same instance through both
// MCP (T-v060-01-06) and HTTP (this file).
type RegistryService interface {
	Register(ctx context.Context, kind registry.Kind, p registry.Profile) (registry.Profile, error)
	Lookup(ctx context.Context, urn string) (registry.Profile, error)
	LookupExternalIDsForURN(ctx context.Context, urn string) ([]registry.ExternalID, error)
	LookupBy(ctx context.Context, kind registry.Kind, externalID, substrate string) (registry.Profile, error)
	Merge(ctx context.Context, urnSrc, urnDst string) (registry.Profile, error)
	Search(ctx context.Context, kind registry.Kind, f registry.Filter) ([]registry.Profile, error)
	UpdateSelf(ctx context.Context, urn string, patch registry.UpdatePatch) (registry.Profile, error)
	Deregister(ctx context.Context, urn string) (registry.Profile, error)
	Sync(ctx context.Context, urn string) (registry.Profile, error)
	BootstrapFromCatalog(ctx context.Context, catalogRoot string, force bool) (registry.BootstrapReport, error)
	BackfillTetherExternalIDs(ctx context.Context, catalogRoot string) (int, error)
	BootstrapFromCerberus(ctx context.Context, cerberusHome string, force bool, writeBack bool) (registry.BootstrapReport, error)
	// RuntimeBinding methods (T07, messaging vNext): the published-local
	// bridge registration surface. See bindings.go.
	LeaseBinding(ctx context.Context, targetURN, sessionID, hostID, attemptID string, capabilities []string, visibility registry.PublicationVisibility, ttl time.Duration) (registry.RuntimeBinding, error)
	LeaseBindingUnlessVisibility(ctx context.Context, targetURN, sessionID, hostID, attemptID string, capabilities []string, visibility registry.PublicationVisibility, ttl time.Duration, blocked ...registry.PublicationVisibility) (registry.RuntimeBinding, error)
	RenewBindingLease(ctx context.Context, bindingID string, ttl time.Duration) (registry.RuntimeBinding, error)
	RevokeBinding(ctx context.Context, bindingID string) error
	CurrentBinding(ctx context.Context, targetURN string) (registry.RuntimeBinding, error)
	ListBindingsForTarget(ctx context.Context, targetURN string) ([]registry.RuntimeBinding, error)
	// Scoped role/slot bindings (T04, exposed in T08). See scoped_bindings.go.
	SetScopedBinding(ctx context.Context, scope, slot string, targetURNs []string, relationship json.RawMessage, createdBy string) (registry.ScopedBinding, error)
	ResolveScopedBinding(ctx context.Context, scope, slot string) (registry.ScopedBinding, error)
	ResolveScopedBindingSingle(ctx context.Context, scope, slot string) (string, registry.ScopedBinding, error)
	ListScopedBindingRevisions(ctx context.Context, scope, slot string) ([]registry.ScopedBinding, error)
}

// kindFromSegment translates the plural URL segment to the singular
// registry.Kind. Returns ok=false for unknown segments so handlers can
// surface a 404 rather than mis-routing.
func kindFromSegment(seg string) (registry.Kind, bool) {
	switch seg {
	case "agents":
		return registry.KindAgent, true
	case "projects":
		return registry.KindProject, true
	case "groups":
		return registry.KindGroup, true
	default:
		return "", false
	}
}

// pluralForKind is the inverse of kindFromSegment, used as the JSON key
// in list-response envelopes (e.g. `{"agents": [...]}` matching the
// catalog convention in catalog.go).
func pluralForKind(k registry.Kind) string {
	switch k {
	case registry.KindAgent:
		return "agents"
	case registry.KindProject:
		return "projects"
	default:
		return string(k) + "s"
	}
}

func singularForKind(k registry.Kind) string {
	return string(k)
}

// registerRegistryRoutes mounts the /registry/ tree onto mux. The route
// is only attached when Server.Registry is non-nil; absent the dep, the
// daemon falls through to its 404 default (matches the Catalog/Broker
// convention in this package).
//
// `/registry/bootstrap` is mounted as a discrete handler (not under the
// `/registry/{kind}/...` dispatcher) because "bootstrap" isn't a kind —
// the verb operates on the whole catalog. Stdlib ServeMux longest-prefix
// match routes the exact path here before the `/registry/` prefix scoop.
func (s *Server) registerRegistryRoutes(mux *http.ServeMux) {
	if s.Registry == nil {
		return
	}
	mux.HandleFunc("/registry/bootstrap", s.handleRegistryBootstrap)
	mux.HandleFunc("/registry/bindings", s.handleBindingsCollection)
	mux.HandleFunc("/registry/bindings/", s.handleBindingsItem)
	mux.HandleFunc("/registry/", s.handleRegistry)
}

// handleRegistry is the single entry point for every /registry/... route.
// It parses (kind, urn, action) from the URL path and dispatches to the
// per-verb helpers. The structure mirrors broker.handleEnvelopesItem.
//
// Path shapes:
//
//	/registry/{kind}                      → collection (POST register, GET search)
//	/registry/{kind}/{urn}                → item (GET lookup, PATCH update, DELETE deregister)
//	/registry/{kind}/{urn}/sync           → action (POST sync)
//
// The {urn} segment is the full msg:// URN as a single (URL-encoded)
// path component. Callers MUST url.PathEscape it before assembly; the
// handler url.PathUnescapes before passing to the service. Because Mux
// URNs contain ':' and '/' characters, escaping is non-optional.
func (s *Server) handleRegistry(w http.ResponseWriter, r *http.Request) {
	// Use the escaped path so URN segments containing %2F survive splitting.
	// The URN scheme is msg://agent/agent-mux/agt_xxx — without using
	// EscapedPath, Go's stdlib decodes the slashes inside the URN before
	// the handler sees the path, which would mis-split the segments.
	escaped := r.URL.EscapedPath()
	rest := strings.TrimPrefix(escaped, "/registry/")
	if rest == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "registry kind required")
		return
	}
	// Split into at most three segments: kind, urn (still URL-escaped),
	// action.
	parts := strings.SplitN(rest, "/", 3)
	kindSeg := parts[0]
	kind, ok := kindFromSegment(kindSeg)
	if !ok {
		writeError(w, http.StatusNotFound, CodeNotFound,
			"unknown registry kind "+kindSeg+" (supported: agents, projects)")
		return
	}

	switch len(parts) {
	case 1:
		// Collection routes.
		switch r.Method {
		case http.MethodPost:
			s.handleRegistryRegister(w, r, kind)
		case http.MethodGet:
			s.handleRegistrySearch(w, r, kind)
		default:
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
				"method not allowed on /registry/"+kindSeg)
		}
	case 2:
		// Item routes — parts[1] is the URL-escaped URN.
		urn, err := url.PathUnescape(parts[1])
		if err != nil || urn == "" {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest,
				"malformed urn in path")
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.handleRegistryLookup(w, r, urn)
		case http.MethodPatch:
			s.handleRegistryUpdate(w, r, urn)
		case http.MethodDelete:
			s.handleRegistryDeregister(w, r, urn)
		default:
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
				"method not allowed on registry item")
		}
	case 3:
		// Action routes — parts[1] = urn, parts[2] = action.
		urn, err := url.PathUnescape(parts[1])
		if err != nil || urn == "" {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest,
				"malformed urn in path")
			return
		}
		switch parts[2] {
		case "sync":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
					"method not allowed on /registry/.../sync")
				return
			}
			s.handleRegistrySync(w, r, urn)
		case "merge":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
					"method not allowed on /registry/.../merge")
				return
			}
			s.handleRegistryMerge(w, r, urn)
		default:
			writeError(w, http.StatusNotFound, CodeNotFound,
				"unknown registry action "+parts[2])
		}
	}
}

type registryMergeRequest struct {
	Into string `json:"into"`
}

// handleRegistryRegister services POST /registry/{kind}. Body is a
// Profile JSON. Kind/CreatedAt/UpdatedAt/MuxInstanceID are stripped here
// so the storage path's defaults apply. A non-empty URN is NOT stripped —
// Service.Register rejects it with ErrInvalidRequest (400) so callers
// learn the contract (server mints URNs; never accepted from input).
// Response: 201 + canonical Profile.
func (s *Server) handleRegistryRegister(w http.ResponseWriter, r *http.Request, kind registry.Kind) {
	var p registry.Profile
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"invalid request body: "+err.Error())
		return
	}
	// Strip caller-supplied identity / timestamp fields so the storage
	// path's defaults apply. Service.Register already rejects a non-empty
	// caller URN with ErrInvalidRequest; everything else just resets.
	p.Kind = ""
	p.CreatedAt = noTime()
	p.UpdatedAt = noTime()
	p.MuxInstanceID = ""

	out, err := s.Registry.Register(r.Context(), kind, p)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// handleRegistrySearch services GET /registry/{kind}. Filter fields are
// drawn from query params; status defaults to active. Empty result is
// `{"<kind-plural>": []}` (non-null slice, matches the catalog
// convention in catalog.go).
//
// Responses are redacted (see redactProfile) -- Search is genuinely
// public discovery: unlike whoami's targeted, self-asserted ?as= lookup,
// it requires no identity assertion at all and lets any same-host caller
// browse every registered row.
func (s *Server) handleRegistrySearch(w http.ResponseWriter, r *http.Request, kind registry.Kind) {
	q := r.URL.Query()
	if externalID := q.Get("external_id"); externalID != "" {
		out, err := s.Registry.LookupBy(r.Context(), kind, externalID, q.Get("substrate"))
		if err != nil {
			writeRegistryError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			singularForKind(kind): redactProfile(out),
		})
		return
	}
	f := registry.Filter{
		Role:       q.Get("role"),
		Title:      q.Get("title"),
		Project:    q.Get("project"),
		Capability: q.Get("capability"),
		SkillName:  q.Get("skill_name"),
		Status:     q.Get("status"),
	}
	out, err := s.Registry.Search(r.Context(), kind, f)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	// Single-key envelope keyed on the plural form — `{"agents": [...]}`
	// or `{"projects": [...]}` — matching the catalog list pattern.
	// redactProfiles always returns a non-nil slice, preserving the
	// non-null-empty-result convention even when out is nil.
	writeJSON(w, http.StatusOK, map[string]any{
		pluralForKind(kind): redactProfiles(out),
	})
}

// handleRegistryLookup services GET /registry/{kind}/{urn}. Both active
// and soft-deleted rows are returned (callers may need to inspect a
// deprecated row's metadata). The kind segment is validated by the
// router but NOT cross-checked against the row's actual kind — a URN
// minted as an agent fetched under /registry/projects/... still returns
// the row. The kind in the path is a routing convenience, not an
// authorization check.
//
// Response is redacted (see redactProfile) for the same "public
// discovery" reason as Search — Lookup takes no ?as= and performs no
// ownership check, so any URN is fetchable by any same-host caller.
func (s *Server) handleRegistryLookup(w http.ResponseWriter, r *http.Request, urn string) {
	out, err := s.Registry.Lookup(r.Context(), urn)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, redactProfile(out))
}

// redactedProfile is the wire shape Search and Lookup ("public discovery"
// -- unscoped browsing, no identity assertion) return in place of the
// full registry.Profile. T09 acceptance #3 ("privacy tests redact
// secrets/private native metadata on public discovery") targets exactly
// four fields this type drops: Callback (a scheme+target pointer that is
// frequently a filesystem path -- T02's own acceptance criteria calls
// this out by name: "leaking provider IDs, paths and secrets"),
// HostAddress (an internal network address), KindMeta (arbitrary
// per-kind operational JSON -- the "private native metadata" the
// acceptance text names), and ExternalIDs (the "provider IDs" T02
// prohibits leaking). Fields are dropped entirely rather than replaced
// with placeholders: a caller who legitimately needs them has a route
// that provides them at full fidelity -- whoami's targeted ?as= query
// (architecture explicitly wants self-discovery to return "identity
// mappings... and host"), or Register/UpdateSelf/Sync/Deregister/Merge,
// none of which this redaction touches.
//
// Caveat (found in this task's own review, not fixed here -- out of
// T09's scope fence): those write-path routes are described above as
// "the row's own owner acting on their own data," but nothing in this
// codebase actually verifies that -- same-host, self-asserted, no-auth-v1
// (ADR 0045) means any caller who knows a URN can PATCH/DELETE/Sync it
// and get the full unredacted Profile back, not just the row's true
// owner. That gap is pre-existing (not introduced by this redaction) and
// matches the declared trust model, but it means Search/Lookup redaction
// alone is a much narrower privacy improvement in practice than
// "redacted on public discovery" implies -- the same fields remain one
// PATCH away for anyone who already has the URN. Left as a follow-up
// (an ownership/ADR question, not a routine fix) rather than expanded
// here.
type redactedProfile struct {
	URN           string           `json:"urn"`
	Kind          registry.Kind    `json:"kind"`
	MuxInstanceID string           `json:"mux_instance_id"`
	DisplayName   string           `json:"display_name"`
	Title         string           `json:"title,omitempty"`
	Role          string           `json:"role,omitempty"`
	Description   string           `json:"description,omitempty"`
	Avatar        string           `json:"avatar,omitempty"`
	Project       string           `json:"project,omitempty"`
	Status        registry.Status  `json:"status"`
	CachedAt      *time.Time       `json:"cached_at,omitempty"`
	HealthStatus  string           `json:"health_status,omitempty"`
	LastSeenAt    *time.Time       `json:"last_seen_at,omitempty"`
	MergedInto    string           `json:"merged_into,omitempty"`
	LastUpdatedBy string           `json:"last_updated_by,omitempty"`
	Capabilities  []string         `json:"capabilities,omitempty"`
	Skills        []registry.Skill `json:"skills,omitempty"`
	Links         []registry.Link  `json:"links,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

func redactProfile(p registry.Profile) redactedProfile {
	return redactedProfile{
		URN: p.URN, Kind: p.Kind, MuxInstanceID: p.MuxInstanceID,
		DisplayName: p.DisplayName, Title: p.Title, Role: p.Role,
		Description: p.Description, Avatar: p.Avatar, Project: p.Project,
		Status: p.Status, CachedAt: p.CachedAt, HealthStatus: p.HealthStatus,
		LastSeenAt: p.LastSeenAt, MergedInto: p.MergedInto,
		LastUpdatedBy: p.LastUpdatedBy, Capabilities: p.Capabilities,
		Skills: p.Skills, Links: p.Links,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

// redactProfiles always returns a non-nil slice (even for a nil/empty
// input), preserving the `{"agents": []}` non-null-empty-result
// convention handleRegistrySearch's own doc comment establishes.
func redactProfiles(ps []registry.Profile) []redactedProfile {
	out := make([]redactedProfile, len(ps))
	for i, p := range ps {
		out[i] = redactProfile(p)
	}
	return out
}

// handleRegistryUpdate services PATCH /registry/{kind}/{urn}. Body is
// an UpdatePatch (see registry.UpdatePatch godoc for the partial-merge
// semantics). LastUpdatedBy is required at the service layer; missing
// it surfaces as ErrInvalidRequest → 400.
func (s *Server) handleRegistryUpdate(w http.ResponseWriter, r *http.Request, urn string) {
	var patch registry.UpdatePatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"invalid request body: "+err.Error())
		return
	}
	out, err := s.Registry.UpdateSelf(r.Context(), urn, patch)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRegistryDeregister services DELETE /registry/{kind}/{urn}.
// Returns the soft-deleted Profile (status='deprecated'), not 204, so
// callers can read the bumped updated_at without a follow-up GET.
func (s *Server) handleRegistryDeregister(w http.ResponseWriter, r *http.Request, urn string) {
	out, err := s.Registry.Deregister(r.Context(), urn)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRegistryBootstrap services POST /registry/bootstrap. Re-runs the
// catalog importer against the daemon-configured catalog root (set in
// Server.RegistryCatalogRoot at composition time). On force=true,
// existing rows are patched + cached_at is bumped; on force=false, the
// run is no-op-skipping for already-imported rows.
//
// Request body is intentionally empty — the only parameter is `force`,
// passed as a query string (`?force=true`). Future params (per-kind
// subset, dry-run) can extend the same query-string surface without a
// body schema migration.
//
// Response is the BootstrapReport as JSON. Per-file errors are NOT a
// 4xx/5xx response — the bootstrap by design treats them as per-row
// failures and continues. Operators inspect report.Errors in the
// response body to surface the bad files.
//
// When the daemon is configured without a catalog root (impossible in
// production but a normal in-process-test shape), the endpoint returns
// 503 with internal_error: the daemon "can run" but has nothing to
// import. A test wiring an empty catalog should set RegistryCatalogRoot
// to a temp dir, not leave it blank.
func (s *Server) handleRegistryBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			"method not allowed on /registry/bootstrap")
		return
	}
	if s.RegistryCatalogRoot == "" {
		writeError(w, http.StatusServiceUnavailable, CodeInternalError,
			"registry: bootstrap: no catalog root configured")
		return
	}
	force := r.URL.Query().Get("force") == "true"
	substrate := r.URL.Query().Get("substrate")
	writeBack := r.URL.Query().Get("write_back") != "false"
	var (
		report registry.BootstrapReport
		err    error
	)
	switch substrate {
	case "", "tether":
		report, err = s.Registry.BootstrapFromCatalog(r.Context(), s.RegistryCatalogRoot, force)
		if err == nil {
			var attached int
			attached, err = s.Registry.BackfillTetherExternalIDs(r.Context(), s.RegistryCatalogRoot)
			report.Attached += attached
		}
	case "cerberus":
		report, err = s.Registry.BootstrapFromCerberus(r.Context(), "", force, writeBack)
	default:
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "unsupported bootstrap substrate "+substrate)
		return
	}
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleRegistrySync services POST /registry/{kind}/{urn}/sync. Two
// success shapes:
//
//   - 204 No Content when the row has no callback configured
//     (registry.ErrNoCallback) — callers treat this as a successful
//     no-op (nothing to refresh).
//   - 200 + refreshed Profile otherwise.
//
// All resolver / payload errors (ErrPayloadInvalid, ErrPayloadTooLarge,
// ErrPathOutsideRoot) surface as 502 Bad Gateway with an internal_error
// envelope — they describe an upstream callback failure, not a flaw in
// the caller's request.
func (s *Server) handleRegistrySync(w http.ResponseWriter, r *http.Request, urn string) {
	out, err := s.Registry.Sync(r.Context(), urn)
	if err != nil {
		if errors.Is(err, registry.ErrNoCallback) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRegistryMerge(w http.ResponseWriter, r *http.Request, urn string) {
	var req registryMergeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	out, err := s.Registry.Merge(r.Context(), urn, req.Into)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// writeRegistryError centralizes the service-error → HTTP-envelope
// mapping documented at the top of this file. Keeping it in one helper
// makes the error code surface easy to audit (and the test matrix
// short).
func writeRegistryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, registry.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
	case errors.Is(err, registry.ErrNoResolver):
		// Operator misconfiguration; surfaced as 400 with the precise
		// scheme name in the message (see file header for the 400-vs-422
		// rationale).
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
	case errors.Is(err, registry.ErrPayloadInvalid),
		errors.Is(err, registry.ErrPayloadTooLarge),
		errors.Is(err, registry.ErrPathOutsideRoot):
		// Upstream callback failures: the row's substrate isn't producing
		// a usable payload. 502 Bad Gateway with an internal_error code —
		// the caller's request was fine.
		writeError(w, http.StatusBadGateway, CodeInternalError, err.Error())
	case errors.Is(err, registry.ErrMintExhausted):
		writeError(w, http.StatusServiceUnavailable, CodeInternalError, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	}
}

// noTime returns a zero time.Time. Using a named helper keeps the
// register path's "wipe caller-supplied fields" intent explicit.
func noTime() (t time.Time) { return t }
