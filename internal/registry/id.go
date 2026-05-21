package registry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
)

// URN + ID constants — D2 (Stripe-style opaque IDs) + D3 (URN scheme).
// v060-01 introduced the agent path. v060-05 extended D3 to the 3-segment
// `msg://group/<authority>/<grp_id>` variant; structural shape (3 segments)
// is preserved across both paths.
const (
	urnScheme        = "msg://"
	defaultAuthority = "agent-mux"

	// URN kind segments (segment 1 after `msg://`).
	urnKindAgent = "agent"
	urnKindGroup = "group"

	// ID prefixes (the first chars of the ID segment).
	agentIDPrefix   = "agt_"
	projectIDPrefix = "prj_"
	groupIDPrefix   = "grp_"

	idSuffixLen = 10
	idAlphabet  = "abcdefghijklmnopqrstuvwxyz0123456789"
	// rejectionThreshold = 256 - (256 % 36) = 252. Bytes ≥ threshold are
	// rejected so the byte → alphabet mapping is unbiased.
	rejectionThreshold = 256 - (256 % len(idAlphabet))
	mintMaxRetries     = 5
)

// ErrMintExhausted is returned when mintMaxRetries consecutive attempts
// all collide with existing URNs. In practice this is overwhelmingly
// unlikely (36^10 ≈ 3.6×10^15 keyspace, low-thousands rows expected) — it
// indicates either a broken entropy source or a malicious caller
// pre-seeding URNs.
var ErrMintExhausted = errors.New("registry: failed to mint a non-colliding URN after retries")

// ErrInvalidURN is returned by ParseRegistryURN for malformed input.
var ErrInvalidURN = errors.New("registry: invalid URN")

// URNExistsFunc is the predicate the minter calls to detect collisions.
// Storage supplies the production implementation against the
// registry_entries table; tests inject a mock.
type URNExistsFunc func(ctx context.Context, urn string) (bool, error)

// URNParts is the parsed form of a registry URN. The URNKind is the
// segment-1 marker (`agent` or `group`) — distinct from the registry
// kind column on storage, which is one of {agent, project, group}.
// Within URNKind=agent the ID prefix (agt_ vs prj_) distinguishes the
// registry kind.
type URNParts struct {
	URNKind   string // "agent" | "group"
	Authority string // typically "agent-mux"
	ID        string // "agt_xxxxxxxxxx" | "prj_xxxxxxxxxx" | "grp_xxxxxxxxxx"
}

// MintAgentURN returns a fresh agent URN of the form
// msg://agent/agent-mux/agt_<10alnum>. exists is consulted up to
// mintMaxRetries times; if every attempt collides, ErrMintExhausted is
// returned.
func MintAgentURN(ctx context.Context, exists URNExistsFunc) (string, error) {
	return mintURN(ctx, rand.Reader, urnKindAgent, defaultAuthority, agentIDPrefix, exists)
}

// MintProjectURN mirrors MintAgentURN with the prj_ prefix; the URN kind
// segment is still `agent` (projects share the agent URN path — only the
// registry-storage kind column distinguishes them).
func MintProjectURN(ctx context.Context, exists URNExistsFunc) (string, error) {
	return mintURN(ctx, rand.Reader, urnKindAgent, defaultAuthority, projectIDPrefix, exists)
}

// MintGroupURN returns a fresh group URN of the form
// msg://group/<authority>/grp_<10alnum>. authority defaults to
// `agent-mux` when empty (single-mux v1 deployments).
//
// The 3-segment shape preserves ADR-0023 §1 canonical URN structure and
// keeps groups federation-routable via ADR-0040's Router. Note: the
// closed AddressKind enum in go-messaging v0.2.1 does not include `group`;
// group URNs are Tether-locally parsed via ParseRegistryURN and never
// traverse the /messages/* boundary that calls go-messaging.ParseURN.
// Cross-substrate group routing lands when go-messaging is bumped.
func MintGroupURN(ctx context.Context, authority string, exists URNExistsFunc) (string, error) {
	if authority == "" {
		authority = defaultAuthority
	}
	return mintURN(ctx, rand.Reader, urnKindGroup, authority, groupIDPrefix, exists)
}

// ParseRegistryURN parses a 3-segment registry URN
// (`msg://<kind>/<authority>/<id>`). Accepts URNKind ∈ {agent, group}
// and validates the ID-prefix matches the URN kind. Returns
// ErrInvalidURN for any malformed input.
func ParseRegistryURN(urn string) (URNParts, error) {
	rest, ok := strings.CutPrefix(urn, urnScheme)
	if !ok {
		return URNParts{}, fmt.Errorf("%w: missing scheme prefix %q", ErrInvalidURN, urnScheme)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return URNParts{}, fmt.Errorf("%w: want 3 path segments, got %d", ErrInvalidURN, len(parts))
	}
	urnKind, authority, id := parts[0], parts[1], parts[2]
	if authority == "" || id == "" {
		return URNParts{}, fmt.Errorf("%w: empty authority or id segment", ErrInvalidURN)
	}
	switch urnKind {
	case urnKindAgent:
		if !strings.HasPrefix(id, agentIDPrefix) && !strings.HasPrefix(id, projectIDPrefix) {
			return URNParts{}, fmt.Errorf("%w: agent URN id must start with %q or %q", ErrInvalidURN, agentIDPrefix, projectIDPrefix)
		}
	case urnKindGroup:
		if !strings.HasPrefix(id, groupIDPrefix) {
			return URNParts{}, fmt.Errorf("%w: group URN id must start with %q", ErrInvalidURN, groupIDPrefix)
		}
	default:
		return URNParts{}, fmt.Errorf("%w: unknown URN kind %q (want %q or %q)", ErrInvalidURN, urnKind, urnKindAgent, urnKindGroup)
	}
	return URNParts{URNKind: urnKind, Authority: authority, ID: id}, nil
}

// IsGroupURN reports whether urn is a well-formed group URN
// (`msg://group/<authority>/grp_<id>`). Convenience wrapper for callers
// that only need the dispatch bit.
func IsGroupURN(urn string) bool {
	p, err := ParseRegistryURN(urn)
	return err == nil && p.URNKind == urnKindGroup
}

// mintURN is the testable core. src is the entropy source — tests pass a
// deterministic bytes.Reader to force collisions or exhaustion. ctx is
// honored between attempts so a cancellation aborts a retry loop
// promptly. urnKind + authority compose the URN path; idPrefix is the
// kind-marker chars (agt_/prj_/grp_).
func mintURN(ctx context.Context, src io.Reader, urnKind, authority, idPrefix string, exists URNExistsFunc) (string, error) {
	for range mintMaxRetries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		suffix, err := randSuffix(src, idSuffixLen)
		if err != nil {
			return "", fmt.Errorf("registry: random suffix: %w", err)
		}
		urn := urnScheme + urnKind + "/" + authority + "/" + idPrefix + suffix
		collides, err := exists(ctx, urn)
		if err != nil {
			return "", fmt.Errorf("registry: collision check: %w", err)
		}
		if !collides {
			return urn, nil
		}
	}
	return "", ErrMintExhausted
}

// randSuffix reads bytes from src and rejection-samples them into the
// 36-character alphabet. Reads one byte at a time so a deterministic
// test source produces deterministic output.
func randSuffix(src io.Reader, n int) (string, error) {
	out := make([]byte, 0, n)
	one := make([]byte, 1)
	for len(out) < n {
		if _, err := io.ReadFull(src, one); err != nil {
			return "", err
		}
		if int(one[0]) >= rejectionThreshold {
			continue
		}
		out = append(out, idAlphabet[int(one[0])%len(idAlphabet)])
	}
	return string(out), nil
}
