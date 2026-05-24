package client

// registry_client.go — typed Go client for the daemon's /registry/...
// federation directory routes (T-v060-01-05). Mirrors the HTTP surface
// defined in internal/api/registry.go.
//
// Error mapping: HTTP 404 → wrapped registry.ErrNotFound; HTTP 400 →
// wrapped registry.ErrInvalidRequest; other 4xx/5xx → a wrapped error
// that surfaces the response body. Connection-level failures wrap
// ErrDaemonUnreachable, matching the rest of this package.
//
// Sync's two success shapes (204 No Content for "no callback", 200 +
// refreshed Profile otherwise) are surfaced as a third return value
// `synced bool`. Callers test that flag to distinguish a successful
// no-op from a refreshed row without inspecting the Profile's
// CachedAt timestamp.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/hollis-labs/tether/internal/registry"
)

// RegistryClient is the typed handle for the daemon's /registry tree.
// Get one via Client.Registry(); methods accept the same registry.Kind
// values used internally and translate to plural URL segments at the
// wire boundary.
type RegistryClient struct {
	c *Client
}

// Registry returns a RegistryClient bound to this Client. The accessor
// is cheap — no allocation beyond the small struct — so callers can
// re-fetch it inline rather than caching.
func (c *Client) Registry() *RegistryClient {
	return &RegistryClient{c: c}
}

// Register POSTs a Profile under /registry/{kind}. The server mints the
// URN; the server REJECTS a non-empty caller-supplied URN with
// registry.ErrInvalidRequest (this is a contract, not a server-side
// strip). Kind, MuxInstanceID, CreatedAt, UpdatedAt are server-assigned
// and the HTTP handler strips them defensively before storage. Returns
// the canonical Profile.
//
// We don't pre-strip p.URN here either — if a caller sets it, the 400
// teaches them the contract.
func (rc *RegistryClient) Register(ctx context.Context, kind registry.Kind, p registry.Profile) (registry.Profile, error) {
	seg, err := pluralSegment(kind)
	if err != nil {
		return registry.Profile{}, err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return registry.Profile{}, fmt.Errorf("marshal profile: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		rc.c.baseURL+"/registry/"+seg, bytes.NewReader(body))
	if err != nil {
		return registry.Profile{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rc.c.http.Do(req)
	if err != nil {
		return registry.Profile{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusCreated {
		return registry.Profile{}, readRegistryError(resp)
	}
	var out registry.Profile
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.Profile{}, fmt.Errorf("decode register response: %w", err)
	}
	return out, nil
}

// Lookup GETs /registry/{kind}/{urn}. The kind is derived from the URN
// prefix so callers don't need to repeat it; this matches the MCP
// surface where lookup-by-urn is the natural shape. 404 returns a
// wrapped registry.ErrNotFound.
func (rc *RegistryClient) Lookup(ctx context.Context, urn string) (registry.Profile, error) {
	seg, err := kindSegmentFromURN(urn)
	if err != nil {
		return registry.Profile{}, err
	}
	path := "/registry/" + seg + "/" + url.PathEscape(urn)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rc.c.baseURL+path, nil)
	if err != nil {
		return registry.Profile{}, err
	}
	resp, err := rc.c.http.Do(req)
	if err != nil {
		return registry.Profile{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return registry.Profile{}, readRegistryError(resp)
	}
	var out registry.Profile
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.Profile{}, fmt.Errorf("decode lookup response: %w", err)
	}
	return out, nil
}

// LookupBy resolves a substrate-local identifier to one registry profile.
func (rc *RegistryClient) LookupBy(ctx context.Context, kind registry.Kind, externalID, substrate string) (registry.Profile, error) {
	seg, err := pluralSegment(kind)
	if err != nil {
		return registry.Profile{}, err
	}
	q := url.Values{}
	q.Set("external_id", externalID)
	if substrate != "" {
		q.Set("substrate", substrate)
	}
	path := "/registry/" + seg + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rc.c.baseURL+path, nil)
	if err != nil {
		return registry.Profile{}, err
	}
	resp, err := rc.c.http.Do(req)
	if err != nil {
		return registry.Profile{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return registry.Profile{}, readRegistryError(resp)
	}
	var env map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return registry.Profile{}, fmt.Errorf("decode lookup-by envelope: %w", err)
	}
	raw, ok := env[string(kind)]
	if !ok {
		return registry.Profile{}, fmt.Errorf("decode lookup-by envelope: missing key %q", kind)
	}
	var out registry.Profile
	if err := json.Unmarshal(raw, &out); err != nil {
		return registry.Profile{}, fmt.Errorf("decode lookup-by profile: %w", err)
	}
	return out, nil
}

// Search GETs /registry/{kind} with the Filter encoded as query params.
// Empty Filter returns all active rows of that kind. Returns a
// non-nil zero-length slice when no rows match.
func (rc *RegistryClient) Search(ctx context.Context, kind registry.Kind, f registry.Filter) ([]registry.Profile, error) {
	seg, err := pluralSegment(kind)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if f.Role != "" {
		q.Set("role", f.Role)
	}
	if f.Title != "" {
		q.Set("title", f.Title)
	}
	if f.Project != "" {
		q.Set("project", f.Project)
	}
	if f.Capability != "" {
		q.Set("capability", f.Capability)
	}
	if f.SkillName != "" {
		q.Set("skill_name", f.SkillName)
	}
	if f.Status != "" {
		q.Set("status", f.Status)
	}
	path := "/registry/" + seg
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rc.c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := rc.c.http.Do(req)
	if err != nil {
		return nil, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, readRegistryError(resp)
	}
	// Server emits {"<kind-plural>": [...]} — decode into a generic
	// envelope and pick the right key.
	var env map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode search envelope: %w", err)
	}
	raw, ok := env[seg]
	if !ok {
		// Defensive: a server emitting the wrong key shape should not
		// crash; return an empty slice rather than panicking.
		return []registry.Profile{}, nil
	}
	var out []registry.Profile
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode search list: %w", err)
	}
	if out == nil {
		out = []registry.Profile{}
	}
	return out, nil
}

// UpdateSelf PATCHes /registry/{kind}/{urn} with the partial-merge
// patch. LastUpdatedBy is required at the server; an absent value is
// surfaced as a wrapped registry.ErrInvalidRequest.
func (rc *RegistryClient) UpdateSelf(ctx context.Context, urn string, patch registry.UpdatePatch) (registry.Profile, error) {
	seg, err := kindSegmentFromURN(urn)
	if err != nil {
		return registry.Profile{}, err
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return registry.Profile{}, fmt.Errorf("marshal patch: %w", err)
	}
	path := "/registry/" + seg + "/" + url.PathEscape(urn)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, rc.c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return registry.Profile{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rc.c.http.Do(req)
	if err != nil {
		return registry.Profile{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return registry.Profile{}, readRegistryError(resp)
	}
	var out registry.Profile
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.Profile{}, fmt.Errorf("decode update response: %w", err)
	}
	return out, nil
}

// Deregister DELETEs /registry/{kind}/{urn}. Returns the soft-deleted
// Profile (status='deprecated') so callers can read the bumped
// updated_at without a follow-up GET.
func (rc *RegistryClient) Deregister(ctx context.Context, urn string) (registry.Profile, error) {
	seg, err := kindSegmentFromURN(urn)
	if err != nil {
		return registry.Profile{}, err
	}
	path := "/registry/" + seg + "/" + url.PathEscape(urn)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, rc.c.baseURL+path, nil)
	if err != nil {
		return registry.Profile{}, err
	}
	resp, err := rc.c.http.Do(req)
	if err != nil {
		return registry.Profile{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return registry.Profile{}, readRegistryError(resp)
	}
	var out registry.Profile
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.Profile{}, fmt.Errorf("decode deregister response: %w", err)
	}
	return out, nil
}

// Merge POSTs /registry/{kind}/{urn-src}/merge with the destination URN and
// returns the canonical destination profile.
func (rc *RegistryClient) Merge(ctx context.Context, urnSrc, urnDst string) (registry.Profile, error) {
	seg, err := kindSegmentFromURN(urnSrc)
	if err != nil {
		return registry.Profile{}, err
	}
	body, err := json.Marshal(map[string]string{"into": urnDst})
	if err != nil {
		return registry.Profile{}, fmt.Errorf("marshal merge request: %w", err)
	}
	path := "/registry/" + seg + "/" + url.PathEscape(urnSrc) + "/merge"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rc.c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return registry.Profile{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rc.c.http.Do(req)
	if err != nil {
		return registry.Profile{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return registry.Profile{}, readRegistryError(resp)
	}
	var out registry.Profile
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.Profile{}, fmt.Errorf("decode merge response: %w", err)
	}
	return out, nil
}

// Sync POSTs /registry/{kind}/{urn}/sync. Returns (Profile{}, false,
// nil) on 204 No Content (row has no callback configured) so callers
// can distinguish "no-op" from "refreshed". On 200, returns the
// refreshed Profile + synced=true.
func (rc *RegistryClient) Sync(ctx context.Context, urn string) (registry.Profile, bool, error) {
	seg, err := kindSegmentFromURN(urn)
	if err != nil {
		return registry.Profile{}, false, err
	}
	path := "/registry/" + seg + "/" + url.PathEscape(urn) + "/sync"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rc.c.baseURL+path, nil)
	if err != nil {
		return registry.Profile{}, false, err
	}
	resp, err := rc.c.http.Do(req)
	if err != nil {
		return registry.Profile{}, false, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	switch resp.StatusCode {
	case http.StatusNoContent:
		return registry.Profile{}, false, nil
	case http.StatusOK:
		var out registry.Profile
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return registry.Profile{}, false, fmt.Errorf("decode sync response: %w", err)
		}
		return out, true, nil
	default:
		return registry.Profile{}, false, readRegistryError(resp)
	}
}

// Bootstrap POSTs /registry/bootstrap. Re-runs the daemon's catalog
// importer against the configured catalog root; force=true patches +
// stamps existing rows where force=false leaves them alone.
//
// Returns the BootstrapReport directly so callers can render counts +
// inspect per-file errors. Per-file errors live inside report.Errors;
// HTTP-level failures (5xx, network, etc.) come back as the second
// return.
func (rc *RegistryClient) Bootstrap(ctx context.Context, force bool, substrate string, writeBack bool) (registry.BootstrapReport, error) {
	path := "/registry/bootstrap"
	q := url.Values{}
	if force {
		q.Set("force", "true")
	}
	if substrate != "" {
		q.Set("substrate", substrate)
	}
	if !writeBack {
		q.Set("write_back", "false")
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rc.c.baseURL+path, nil)
	if err != nil {
		return registry.BootstrapReport{}, err
	}
	resp, err := rc.c.http.Do(req)
	if err != nil {
		return registry.BootstrapReport{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return registry.BootstrapReport{}, readRegistryError(resp)
	}
	var out registry.BootstrapReport
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.BootstrapReport{}, fmt.Errorf("decode bootstrap response: %w", err)
	}
	return out, nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// pluralSegment translates a registry.Kind to its URL segment. Mirrors
// the inverse table in internal/api/registry.go (kindFromSegment); kept
// in sync manually since dragging the api package into client tests
// would create an import cycle.
func pluralSegment(k registry.Kind) (string, error) {
	switch k {
	case registry.KindAgent:
		return "agents", nil
	case registry.KindProject:
		return "projects", nil
	case registry.KindGroup:
		return "groups", nil
	default:
		return "", fmt.Errorf("registry client: unsupported kind %q", string(k))
	}
}

// kindSegmentFromURN derives the URL segment from the URN suffix. URN
// prefixes are stable (D2): "agt_" → agents, "prj_" → projects.
func kindSegmentFromURN(urn string) (string, error) {
	const prefix = "msg://agent/agent-mux/"
	tail := strings.TrimPrefix(urn, prefix)
	switch {
	case strings.HasPrefix(tail, "agt_"):
		return "agents", nil
	case strings.HasPrefix(tail, "prj_"):
		return "projects", nil
	default:
		return "", fmt.Errorf("registry client: cannot infer kind from URN %q", urn)
	}
}

// readRegistryError reads a daemon error envelope and maps it to a
// typed registry error where possible. Falls back to a generic wrapped
// error that surfaces the body for unknown codes (matches readError's
// shape so callers can still grep for "daemon NNN (code): message").
func readRegistryError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)

	// Type-map the well-known status codes before falling through to the
	// generic error. errors.Is at the caller wires up cleanly.
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("daemon %d (%s): %s: %w", resp.StatusCode, env.Error.Code, env.Error.Message, registry.ErrNotFound)
	case http.StatusBadRequest:
		return fmt.Errorf("daemon %d (%s): %s: %w", resp.StatusCode, env.Error.Code, env.Error.Message, registry.ErrInvalidRequest)
	}
	if env.Error.Message != "" {
		return fmt.Errorf("daemon %d (%s): %s", resp.StatusCode, env.Error.Code, env.Error.Message)
	}
	return fmt.Errorf("daemon %d: %s", resp.StatusCode, string(body))
}
