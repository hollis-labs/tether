package authissuer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

const (
	BasaliskStatusAudience   = identity.BasaliskStatusAudience
	TachyonAssuranceAudience = identity.TachyonAssuranceAudience
)

func isWorkloadAudience(audience string) bool {
	return audience == BasaliskStatusAudience || audience == TachyonAssuranceAudience
}

// Bound new workload state and its synchronous persistence cost. Clients reuse
// their short-lived JWT; current trust is queried for each use, not re-minted.
const (
	MaxWorkloadMintsPerCredential = 32
	MaxWorkloadMints              = 1024
)

var ErrWorkloadMintLimit = errors.New("authissuer: workload mint capacity reached")

// WorkloadMint retains only nonsecret metadata until expiry. Legacy tokens
// without an exact credential-bound mint record cannot pass current trust.
type WorkloadMint struct {
	Claims       Claims `json:"claims"`
	KID          string `json:"kid"`
	CredentialID int64  `json:"credential_id"`
}

// WorkloadQuery is copied from a locally verified shared-auth Principal.
// It is never evidence of JWT signature validity on its own.
type WorkloadQuery struct {
	Issuer    string    `json:"issuer"`
	Subject   string    `json:"subject"`
	Kind      string    `json:"kind"`
	Audience  string    `json:"audience"`
	Scopes    []string  `json:"scopes"`
	TokenID   string    `json:"token_id"`
	Parent    string    `json:"parent"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *State) trackWorkload(m WorkloadMint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if len(s.doc.Workloads) >= MaxWorkloadMints {
		return ErrWorkloadMintLimit
	}
	live := 0
	for _, existing := range s.doc.Workloads {
		if existing.CredentialID == m.CredentialID {
			live++
		}
	}
	if live >= MaxWorkloadMintsPerCredential {
		return ErrWorkloadMintLimit
	}
	if s.doc.Workloads == nil {
		s.doc.Workloads = make(map[string]WorkloadMint)
	}
	s.doc.Workloads[m.Claims.JTI] = m
	// Include direct workload mints in the existing parent-rotation revocation
	// operation as well as the credential-specific lifecycle lookup.
	before := len(s.doc.Minted)
	s.doc.Minted = append(s.doc.Minted, mintedEntry{JTI: m.Claims.JTI, Par: m.Claims.Par, Exp: time.Unix(m.Claims.Exp, 0)})
	if err := s.saveLocked(); err != nil {
		delete(s.doc.Workloads, m.Claims.JTI)
		s.doc.Minted = s.doc.Minted[:before]
		return err
	}
	return nil
}

func (s *State) workload(id string) (WorkloadMint, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.doc.Workloads[id]
	return m, ok
}

// CurrentWorkload checks a narrowly supported direct workload, including its
// minting parent (par == sub), exact credential, current scopes and signing key.
// It supplies no permission to execute an operation or skip JWT verification.
func (i *Issuer) CurrentWorkload(ctx context.Context, ids *identity.Store, q WorkloadQuery) (bool, error) {
	if i == nil || i.State == nil || i.Keys == nil || ids == nil {
		return false, errors.New("current workload trust unavailable")
	}
	m, ok := i.State.workload(q.TokenID)
	c := m.Claims
	if !ok || q.Issuer != i.Name || c.Iss != q.Issuer || q.Subject != c.Sub || q.Kind != c.Kind ||
		!identity.WorkloadScopesAllowed(q.Audience, q.Scopes) || c.Aud != q.Audience || q.Parent != c.Par || c.Par != c.Sub ||
		c.SID != "" || (c.Kind != "device" && c.Kind != "service") || c.JTI != q.TokenID ||
		!q.ExpiresAt.Equal(time.Unix(c.Exp, 0)) ||
		!slices.Equal(q.Scopes, strings.Fields(c.Scope)) || c.Iat <= 0 || c.Exp <= c.Iat || c.Exp-c.Iat > 900 {
		return false, nil
	}
	if err := ids.CheckWorkloadCredential(ctx, m.CredentialID, c.Sub, c.Kind, c.Aud, q.Scopes, i.Now()); err != nil {
		if errors.Is(err, identity.ErrInvalidToken) {
			return false, nil
		}
		return false, err
	}
	// Recheck time after storage waits; no positive expiry leeway.
	now := i.Now().Unix()
	return ctx.Err() == nil && c.Iat <= now && now < c.Exp && i.Keys.current(m.KID) &&
		!i.State.Denied(DenyJTI, c.JTI) && !i.State.Denied(DenySub, c.Sub) && !i.State.Denied(DenySub, c.Par), ctx.Err()
}

func (ks *KeyStore) current(kid string) bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for _, k := range ks.keys {
		if k.KID == kid && (k.RetireAt.IsZero() || ks.now().Before(k.RetireAt)) {
			return true
		}
	}
	return false
}
