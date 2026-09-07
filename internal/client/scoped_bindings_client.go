package client

// scoped_bindings_client.go — typed Go client for the daemon's
// /registry/scoped-bindings routes (T08, messaging vNext). Mirrors
// internal/api/scoped_bindings.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/hollis-labs/tether/internal/registry"
)

// ScopedBindingsClient is the typed handle for the daemon's
// /registry/scoped-bindings tree. Get one via Client.ScopedBindings().
type ScopedBindingsClient struct {
	c *Client
}

// ScopedBindings returns a ScopedBindingsClient bound to this Client.
func (c *Client) ScopedBindings() *ScopedBindingsClient {
	return &ScopedBindingsClient{c: c}
}

// Set POSTs /registry/scoped-bindings, publishing a new revision for
// (scope, slot).
func (sc *ScopedBindingsClient) Set(ctx context.Context, scope, slot string, targetURNs []string, relationship json.RawMessage, createdBy string) (registry.ScopedBinding, error) {
	body, err := json.Marshal(map[string]any{
		"scope": scope, "slot": slot,
		"target_urns":  targetURNs,
		"relationship": relationship,
		"created_by":   createdBy,
	})
	if err != nil {
		return registry.ScopedBinding{}, fmt.Errorf("marshal set scoped binding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sc.c.baseURL+"/registry/scoped-bindings", bytes.NewReader(body))
	if err != nil {
		return registry.ScopedBinding{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sc.c.http.Do(req)
	if err != nil {
		return registry.ScopedBinding{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusCreated {
		return registry.ScopedBinding{}, readScopedBindingError(resp)
	}
	var out registry.ScopedBinding
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.ScopedBinding{}, fmt.Errorf("decode set scoped binding response: %w", err)
	}
	return out, nil
}

// Resolve GETs /registry/scoped-bindings/resolve?scope=&slot=, returning
// the current revision (every target, if more than one).
func (sc *ScopedBindingsClient) Resolve(ctx context.Context, scope, slot string) (registry.ScopedBinding, error) {
	q := url.Values{"scope": {scope}, "slot": {slot}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sc.c.baseURL+"/registry/scoped-bindings/resolve?"+q.Encode(), nil)
	if err != nil {
		return registry.ScopedBinding{}, err
	}
	resp, err := sc.c.http.Do(req)
	if err != nil {
		return registry.ScopedBinding{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return registry.ScopedBinding{}, readScopedBindingError(resp)
	}
	var out registry.ScopedBinding
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.ScopedBinding{}, fmt.Errorf("decode resolve response: %w", err)
	}
	return out, nil
}

// ResolveSingle GETs /registry/scoped-bindings/resolve?...&single=true,
// resolving to exactly one target URN. Returns registry.ErrAmbiguousBinding
// or registry.ErrBindingHasNoTargets (wrapped) when the current revision
// doesn't name exactly one target.
func (sc *ScopedBindingsClient) ResolveSingle(ctx context.Context, scope, slot string) (string, registry.ScopedBinding, error) {
	q := url.Values{"scope": {scope}, "slot": {slot}, "single": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sc.c.baseURL+"/registry/scoped-bindings/resolve?"+q.Encode(), nil)
	if err != nil {
		return "", registry.ScopedBinding{}, err
	}
	resp, err := sc.c.http.Do(req)
	if err != nil {
		return "", registry.ScopedBinding{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return "", registry.ScopedBinding{}, readScopedBindingError(resp)
	}
	var out struct {
		TargetURN string                 `json:"target_urn"`
		Binding   registry.ScopedBinding `json:"binding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", registry.ScopedBinding{}, fmt.Errorf("decode resolve-single response: %w", err)
	}
	return out.TargetURN, out.Binding, nil
}

// ListRevisions GETs /registry/scoped-bindings/revisions?scope=&slot=,
// the full provenance history for (scope, slot), newest first.
func (sc *ScopedBindingsClient) ListRevisions(ctx context.Context, scope, slot string) ([]registry.ScopedBinding, error) {
	q := url.Values{"scope": {scope}, "slot": {slot}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sc.c.baseURL+"/registry/scoped-bindings/revisions?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := sc.c.http.Do(req)
	if err != nil {
		return nil, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, readScopedBindingError(resp)
	}
	var env struct {
		Revisions []registry.ScopedBinding `json:"revisions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode list revisions response: %w", err)
	}
	if env.Revisions == nil {
		env.Revisions = []registry.ScopedBinding{}
	}
	return env.Revisions, nil
}

// readScopedBindingError reads a daemon error envelope and maps it to a
// typed registry error where possible, mirroring readRegistryError's shape.
func readScopedBindingError(resp *http.Response) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)

	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("daemon %d (%s): %s: %w", resp.StatusCode, env.Error.Code, env.Error.Message, registry.ErrNotFound)
	case http.StatusBadRequest:
		return fmt.Errorf("daemon %d (%s): %s: %w", resp.StatusCode, env.Error.Code, env.Error.Message, registry.ErrInvalidRequest)
	case http.StatusConflict:
		// Mirrors bindings_client.go's readBindingError precedent: the
		// two possible conflict sentinels have non-overlapping message
		// text, so a substring match reliably distinguishes them rather
		// than collapsing both into a single untyped error.
		switch {
		case strings.Contains(env.Error.Message, registry.ErrAmbiguousBinding.Error()):
			return fmt.Errorf("daemon %d (%s): %s: %w", resp.StatusCode, env.Error.Code, env.Error.Message, registry.ErrAmbiguousBinding)
		case strings.Contains(env.Error.Message, registry.ErrBindingHasNoTargets.Error()):
			return fmt.Errorf("daemon %d (%s): %s: %w", resp.StatusCode, env.Error.Code, env.Error.Message, registry.ErrBindingHasNoTargets)
		default:
			return fmt.Errorf("daemon %d (%s): %s", resp.StatusCode, env.Error.Code, env.Error.Message)
		}
	}
	if env.Error.Message != "" {
		return fmt.Errorf("daemon %d (%s): %s", resp.StatusCode, env.Error.Code, env.Error.Message)
	}
	return fmt.Errorf("daemon %d", resp.StatusCode)
}
