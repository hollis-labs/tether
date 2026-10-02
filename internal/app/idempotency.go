package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workspace"
)

// Idempotent session create and resume (CW-20260930-0229).
//
// A key binds permanently to the session its first request created. The
// digest is of the request as the caller sent it, never of anything the
// daemon resolved from it: a resume retried after the new session has
// written its own checkpoint must still replay, not conflict because the
// resolver would now pick a newer parent.

// idempotencyLocks serializes requests for one key within the daemon, the
// single writer for its state DB (ADR 0035). The primary key on
// session_idempotency is the backstop.
var idempotencyLocks sync.Map // key -> *sync.Mutex

func (s *Service) lockIdempotencyKey(key string) func() {
	mu, _ := idempotencyLocks.LoadOrStore(key, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// createRequestDigest hashes a create request as sent. File-valued fields
// hash the path, not the file, so the caller alone controls the identity.
func createRequestDigest(in CreateSessionInput) string {
	return requestDigest(struct {
		Op              string               `json:"op"`
		Launch          string               `json:"launch"`
		BootPrompt      string               `json:"boot_prompt"`
		AgentFile       string               `json:"agent_file"`
		AgentInline     string               `json:"agent_inline"`
		BootProfileFile string               `json:"boot_profile"`
		Override        string               `json:"override"`
		PromptAppend    string               `json:"prompt_append"`
		Injection       string               `json:"injection"`
		Route           *launchprofile.Route `json:"route,omitempty"`
	}{
		store.IdempotencyOpCreate, in.LaunchID, in.BootPromptOverride, in.AgentFile, in.AgentInline,
		in.BootProfileFile, in.Override, in.BootPromptAppend, in.Injection, in.Route,
	})
}

// resumeRequestDigest hashes a resume request as sent. Resume takes no
// explicit checkpoint today, so the logical agent is the whole request.
func resumeRequestDigest(logicalAgentID string) string {
	return requestDigest(struct {
		Op             string `json:"op"`
		LogicalAgentID string `json:"logical_agent_id"`
	}{store.IdempotencyOpResume, logicalAgentID})
}

func requestDigest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// A struct of strings always marshals.
		panic(fmt.Sprintf("app: marshal request digest: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// createKeyed is the keyed create: under the key's lock, replay the bound
// session or build a plan and create one bound to the key in the same
// transaction as its row.
func (s *Service) createKeyed(key, digest string, build func() (*launch.Plan, error)) (*Launched, error) {
	unlock := s.lockIdempotencyKey(key)
	defer unlock()
	if launched, err := s.replayIfKeyed(key, store.IdempotencyOpCreate, digest); launched != nil || err != nil {
		return launched, err
	}
	plan, err := build()
	if err != nil {
		return nil, err
	}
	return s.createSessionFromPlan(plan, &store.SessionIdempotency{
		Key: key, Operation: store.IdempotencyOpCreate, RequestDigest: digest,
	})
}

// replayIfKeyed returns the session key is bound to when the stored request
// matches (op, digest), ErrIdempotencyConflict when it does not, and
// (nil, nil) when the key is new. The caller must hold the key's lock.
func (s *Service) replayIfKeyed(key, op, digest string) (*Launched, error) {
	rec, err := s.Store.GetSessionIdempotency(key)
	if err != nil {
		return nil, fmt.Errorf("look up idempotency key: %w", err)
	}
	if rec == nil {
		return nil, nil
	}
	if rec.Operation != op || rec.RequestDigest != digest {
		return nil, fmt.Errorf("%w (key bound to session %s by a %s request)", store.ErrIdempotencyConflict, rec.SessionID, rec.Operation)
	}
	return s.replayedSession(rec.SessionID)
}

// replayedSession returns an existing session as it now stands, whatever its
// state: a failed or stopped session is returned unchanged, never replaced.
func (s *Service) replayedSession(sessionID string) (*Launched, error) {
	row, err := s.Store.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	plan, err := s.Store.GetLaunchPlan(sessionID)
	if err != nil {
		return nil, fmt.Errorf("load launch plan: %w", err)
	}
	return &Launched{
		SessionID:    sessionID,
		Workspace:    workspace.Open(row.Workspace, sessionID),
		Plan:         plan,
		ProviderKind: row.ProviderKind,
		Replayed:     true,
	}, nil
}
