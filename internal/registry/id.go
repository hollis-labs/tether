package registry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// URN + ID constants — D2 (Stripe-style opaque IDs) + D3 (URN scheme).
const (
	urnPrefix       = "msg://agent/agent-mux/"
	agentIDPrefix   = "agt_"
	projectIDPrefix = "prj_"
	idSuffixLen     = 10
	idAlphabet      = "abcdefghijklmnopqrstuvwxyz0123456789"
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

// URNExistsFunc is the predicate the minter calls to detect collisions.
// Storage supplies the production implementation against the
// registry_entries table; tests inject a mock.
type URNExistsFunc func(ctx context.Context, urn string) (bool, error)

// MintAgentURN returns a fresh agent URN of the form
// msg://agent/agent-mux/agt_<10alnum>. exists is consulted up to
// mintMaxRetries times; if every attempt collides, ErrMintExhausted is
// returned.
func MintAgentURN(ctx context.Context, exists URNExistsFunc) (string, error) {
	return mintURN(ctx, rand.Reader, agentIDPrefix, exists)
}

// MintProjectURN mirrors MintAgentURN with the prj_ prefix.
func MintProjectURN(ctx context.Context, exists URNExistsFunc) (string, error) {
	return mintURN(ctx, rand.Reader, projectIDPrefix, exists)
}

// mintURN is the testable core. src is the entropy source — tests pass a
// deterministic bytes.Reader to force collisions or exhaustion. ctx is
// honored between attempts so a cancellation aborts a retry loop
// promptly.
func mintURN(ctx context.Context, src io.Reader, prefix string, exists URNExistsFunc) (string, error) {
	for range mintMaxRetries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		suffix, err := randSuffix(src, idSuffixLen)
		if err != nil {
			return "", fmt.Errorf("registry: random suffix: %w", err)
		}
		urn := urnPrefix + prefix + suffix
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
