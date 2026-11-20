package app

import (
	"context"
	"crypto/ed25519"
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
	key, err := s.runTokens.Key(s.Store.DB())
	if err != nil {
		return nil, err
	}
	c := runtoken.Claims{URN: runURN(row), Session: row.ID, RunID: uuid.NewString(), JTI: uuid.NewString(), Task: row.TaskID.String, Workstream: row.WorkstreamID.String, Expires: time.Now().Add(sessionTokenTTL()).Unix()}
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
	if s == nil || s.Store == nil || !p.Verified || p.SessionID == "" || (p.Kind != "session" && !identity.IsBoundAgentToken(p)) {
		return runtoken.Claims{}, runtoken.ErrInvalid
	}
	key, err := s.runTokens.Key(s.Store.DB())
	if err != nil {
		return runtoken.Claims{}, runtoken.ErrInvalid
	}
	c, err := runtoken.Verify(key.Public().(ed25519.PublicKey), token, time.Now())
	if err != nil {
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
