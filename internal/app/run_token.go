package app

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/runtoken"
	"github.com/hollis-labs/tether/internal/store"
)

func runURN(row *store.SessionRow) string {
	if row.LogicalAgentID != "" {
		return registry.LogicalAgentBindingTarget(row.LogicalAgentID)
	}
	return identity.SessionPrincipalIDPrefix + row.ID
}

// mintRunEnvironment never modifies or persists the launch plan. Parent is
// inherited only from a verified run in the launching request's context.
func (s *Service) mintRunEnvironment(ctx context.Context, row *store.SessionRow) (map[string]string, error) {
	supported := runtoken.HandoffSupported()
	if s.runTokenSupport != nil {
		supported = s.runTokenSupport()
	}
	return s.mintRunEnvironmentFor(ctx, row, supported)
}
func (s *Service) mintRunEnvironmentFor(ctx context.Context, row *store.SessionRow, supported bool) (map[string]string, error) {
	execution := uuid.NewString()
	if !supported {
		return map[string]string{runtoken.IDEnv: execution}, nil
	}
	key, err := s.runTokens.Key(s.Store.RunSigningKeyPath())
	if err != nil {
		return nil, err
	}
	expires, err := s.runTokenExpiry(row, time.Now())
	if err != nil {
		return nil, err
	}
	c := runtoken.Claims{URN: runURN(row), Session: row.ID, RunID: execution, JTI: uuid.NewString(), Task: row.TaskID.String, Workstream: row.WorkstreamID.String, Expires: expires.Unix()}
	if parent, ok := runtoken.FromContext(ctx); ok {
		c.Parent = parent.RunID
	}
	token, err := runtoken.Sign(key, c)
	if err != nil {
		return nil, err
	}
	return map[string]string{runtoken.IDEnv: c.RunID, runtoken.TokenEnv: token}, nil
}

// VerifyRunToken validates correlation against the authenticated session and its
// current server-owned context. Task/workstream reassignment invalidates an old
// signed context instead of overwriting the existing attribution fields.
func (s *Service) VerifyRunToken(token string, p identity.Principal) (runtoken.Claims, error) {
	if s == nil || s.Store == nil || !p.Verified || p.SessionID == "" || p.ID != identity.SessionPrincipalIDPrefix+p.SessionID || (p.Kind != "session" && !identity.IsBoundAgentToken(p)) {
		return runtoken.Claims{}, runtoken.ErrInvalid
	}
	key, err := s.runTokens.Key(s.Store.RunSigningKeyPath())
	if err != nil {
		return runtoken.Claims{}, runtoken.ErrInvalid
	}
	c, err := runtoken.Verify(key.Public().(ed25519.PublicKey), token, time.Now())
	if err != nil {
		return runtoken.Claims{}, runtoken.ErrInvalid
	}
	denied, err := s.Store.RunTokenDenied(context.Background(), c.JTI, c.RunID)
	if err != nil || denied {
		return runtoken.Claims{}, runtoken.ErrInvalid
	}
	row, err := s.Store.GetSession(p.SessionID)
	if err != nil || row == nil || c.Session != p.SessionID || c.URN != runURN(row) || c.Task != row.TaskID.String || c.Workstream != row.WorkstreamID.String {
		return runtoken.Claims{}, runtoken.ErrInvalid
	}
	switch row.State {
	case "completed", "failed", "killed":
		return runtoken.Claims{}, runtoken.ErrInvalid
	}
	return c, nil
}

// Run proofs are fixed to one execution, not the ten-year session bearer lease.
// Long/unbounded sessions must start a new execution after the six-hour cap.
func (s *Service) runTokenExpiry(row *store.SessionRow, now time.Time) (time.Time, error) {
	expires := now.Add(6 * time.Hour)
	plan, err := s.Store.GetLaunchPlan(row.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return expires, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	d, err := plan.Lifecycle.Durations()
	if err != nil {
		return time.Time{}, err
	}
	if d.MaxDuration > 0 {
		// Minting begins a new execution, even for a session preassigned long
		// before launch. Its duration must not start at session creation.
		expires = minTime(expires, now.Add(d.MaxDuration).Add(5*time.Minute))
	}
	return expires, nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// RevokeRunToken permanently denies an exact jti or execution run identifier.
// No signed proof or credential is accepted or retained by this control path.
func (s *Service) RevokeRunToken(ctx context.Context, id string) error {
	return s.Store.RevokeRunToken(ctx, id)
}
