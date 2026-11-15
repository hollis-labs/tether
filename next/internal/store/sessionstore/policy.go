package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

var (
	// ErrSessionMCPPolicyUnavailable is returned when a session has no bound MCP policy, is no
	// longer live, or (for recovery) has no active credential and binding.
	ErrSessionMCPPolicyUnavailable = errors.New("session MCP policy unavailable; resume or relaunch the session")
	// ErrSessionReplacementUnavailable is returned by ClearNativeResume when the source session was
	// replaced by a session other than the one asking.
	ErrSessionReplacementUnavailable = errors.New("session replacement unavailable")
)

// polNotTerminal is the old store's "not terminal" test for MCP policy.
const polNotTerminal = `state NOT IN ('completed','failed','killed')`

// polLiveSessionStates are the states AgentHasLiveSession counts as live.
const polLiveSessionStates = `('created','ready','launching','running','detached')`

// polMillis is the old strftime('%Y-%m-%dT%H:%M:%fZ','now') text, from the store's clock.
func (s *Store) polMillis() string { return s.now().Format("2006-01-02T15:04:05.000Z") }

// SaveSessionMCPPolicy binds policyJSON as the session's MCP policy if none is bound yet and the
// session is live and owned by agentID (the old conditional INSERT ... SELECT ... ON CONFLICT DO
// NOTHING), and returns the policy that is bound after the write, read in the same transaction.
// Validating the policy, and comparing the stored digest with the caller's (the old "already bound"
// error), are the MCP slice's. ErrSessionMCPPolicyUnavailable when nothing is bound (the session is
// unknown, terminal or another agent's). On Postgres a concurrent save waits on the conflicting row
// and then reads the committed one, so every caller sees the single bound policy. Postgres refuses
// a policy jsonb cannot hold (the table's pg_input_is_valid check; a \u0000 escape).
func (s *Store) SaveSessionMCPPolicy(ctx context.Context, sessionID, agentID, policyJSON string) (stored string, err error) {
	err = s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO session_mcp_policy (session_id, policy_json)
 SELECT id, ? FROM sessions WHERE id = ? AND COALESCE(logical_agent_id,'') = ? AND `+polNotTerminal+`
 ON CONFLICT (session_id) DO NOTHING`), policyJSON, sessionID, agentID); err != nil {
			return fmt.Errorf("persist session MCP policy: %w", err)
		}
		stored, _, err = polPolicy(ctx, tx, sessionID)
		return err
	})
	return stored, err
}

// SessionMCPPolicyJSON returns a live session's bound policy JSON and the session's logical agent
// (ErrSessionMCPPolicyUnavailable when none). Parsing, validating and checking the policy's
// identity against the session and agent are the MCP slice's.
func (s *Store) SessionMCPPolicyJSON(ctx context.Context, sessionID string) (policyJSON, agentID string, err error) {
	return polPolicy(ctx, s.c.Reader(), sessionID)
}

func polPolicy(ctx context.Context, q gostore.Querier, sessionID string) (policyJSON, agentID string, err error) {
	err = q.QueryRowContext(ctx, rebind(q, `SELECT p.policy_json, COALESCE(s.logical_agent_id,'') FROM session_mcp_policy p JOIN sessions s ON s.id = p.session_id
 WHERE p.session_id = ? AND s.`+polNotTerminal), sessionID).Scan(&policyJSON, &agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrSessionMCPPolicyUnavailable
	}
	return policyJSON, agentID, err
}

// RecoverySessionMCPPolicyJSON is SessionMCPPolicyJSON for recovery: the session must also hold an
// unexpired, unrevoked session credential and an unexpired, unrevoked binding at its target's
// latest generation (ErrSessionMCPPolicyUnavailable otherwise). The old store tested both in one
// statement; here the credential is read from class A (principals) and the binding from class C,
// two reads, and "now" is the store's clock in the old strftime text.
func (s *Store) RecoverySessionMCPPolicyJSON(ctx context.Context, sessionID string) (policyJSON, agentID string, err error) {
	policyJSON, agentID, err = s.SessionMCPPolicyJSON(ctx, sessionID)
	if err != nil {
		return "", "", err
	}
	now := s.polMillis()
	a := s.all.A().Reader()
	var credential bool
	if err := a.QueryRowContext(ctx, rebind(a, `SELECT EXISTS(SELECT 1 FROM principals WHERE kind = 'session' AND session_id = ?
 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?))`), sessionID, now).Scan(&credential); err != nil {
		return "", "", err
	}
	if !credential {
		return "", "", ErrSessionMCPPolicyUnavailable
	}
	c := s.c.Reader()
	var binding bool
	if err := c.QueryRowContext(ctx, rebind(c, `SELECT EXISTS(SELECT 1 FROM runtime_bindings b WHERE b.session_id = ? AND b.revoked_at IS NULL
 AND (b.lease_expires_at IS NULL OR b.lease_expires_at > ?)
 AND b.generation = (SELECT MAX(generation) FROM runtime_bindings WHERE target_urn = b.target_urn))`), sessionID, now).Scan(&binding); err != nil {
		return "", "", err
	}
	if !binding {
		return "", "", ErrSessionMCPPolicyUnavailable
	}
	return policyJSON, agentID, nil
}

// MCPUpstreamSessionCounts counts live non-API sessions by their policy's upstream_ownership
// ("legacy_proxy", "daemon", anything else or no policy as "unknown"). On Postgres the policy is
// read under the pg_input_is_valid guard.
func (s *Store) MCPUpstreamSessionCounts(ctx context.Context) (map[string]int, error) {
	r := s.c.Reader()
	ownership := `json_extract(p.policy_json,'$.upstream_ownership')`
	if r.Dialect().Name() == gostore.DriverPostgres {
		ownership = `(CASE WHEN pg_input_is_valid(p.policy_json, 'jsonb') THEN p.policy_json::jsonb->>'upstream_ownership' END)`
	}
	rows, err := r.QueryContext(ctx, `SELECT COALESCE(NULLIF(`+ownership+`,''),'unknown'), COUNT(*)
 FROM sessions s LEFT JOIN session_mcp_policy p ON p.session_id = s.id
 WHERE s.state IN ('launching','running','detached') AND s.provider_kind <> 'api'
 GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int{"legacy_proxy": 0, "daemon": 0, "unknown": 0}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		if key != "legacy_proxy" && key != "daemon" {
			key = "unknown"
		}
		counts[key] += n
	}
	return counts, rows.Err()
}

// WorkerPermissionParser is a session's permission-forwarding parser checkpoint.
type WorkerPermissionParser struct {
	Sequence int64
	Spans    string
	Discard  bool
}

// WorkerPermissionParser returns the session's checkpoint (sequence 0, spans "[]" when none).
func (s *Store) WorkerPermissionParser(ctx context.Context, sessionID string) (WorkerPermissionParser, error) {
	state := WorkerPermissionParser{Spans: "[]"}
	r := s.c.Reader()
	var discard int64
	err := r.QueryRowContext(ctx, rebind(r, `SELECT last_seq, spans_json, discard FROM worker_permission_parser WHERE session_id = ?`), sessionID).
		Scan(&state.Sequence, &state.Spans, &discard)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	state.Discard = discard != 0
	return state, err
}

// SaveWorkerPermissionParser stores the checkpoint if it is newer than the stored one (a guarded
// upsert, so racing saves keep the highest sequence).
func (s *Store) SaveWorkerPermissionParser(ctx context.Context, sessionID string, state WorkerPermissionParser) error {
	discard := 0
	if state.Discard {
		discard = 1
	}
	return s.exec(ctx, `INSERT INTO worker_permission_parser (session_id, last_seq, spans_json, discard) VALUES (?, ?, ?, ?)
 ON CONFLICT (session_id) DO UPDATE SET last_seq = excluded.last_seq, spans_json = excluded.spans_json, discard = excluded.discard
 WHERE excluded.last_seq > worker_permission_parser.last_seq`, sessionID, state.Sequence, state.Spans, discard)
}

// SessionActivity is what the stall detector reads from the event log.
type SessionActivity struct {
	LastEventAt       time.Time
	DeniedSinceOutput int
}

// Event kinds SessionActivity counts (events.KindProviderPermissionDenied and
// events.KindSessionTurnOutput in the old tree).
const (
	polKindPermissionDenied = "provider.permission_denied"
	polKindTurnOutput       = "session.turn_output"
)

// SessionActivity returns the session's latest event time and how many permission denials it has
// had since its last turn output, from the class B event log.
func (s *Store) SessionActivity(ctx context.Context, sessionID string) (SessionActivity, error) {
	var act SessionActivity
	b := s.all.B().Reader()
	var last sql.NullString
	if err := b.QueryRowContext(ctx, rebind(b, `SELECT MAX(at) FROM events WHERE session_id = ?`), sessionID).Scan(&last); err != nil {
		return act, fmt.Errorf("session activity: last event: %w", err)
	}
	if last.Valid {
		if t, err := time.Parse(time.RFC3339Nano, last.String); err == nil {
			act.LastEventAt = t
		}
	}
	if err := b.QueryRowContext(ctx, rebind(b, `SELECT COUNT(*) FROM events WHERE session_id = ? AND kind = ?
 AND id > COALESCE((SELECT MAX(id) FROM events WHERE session_id = ? AND kind = ?), 0)`),
		sessionID, polKindPermissionDenied, sessionID, polKindTurnOutput).Scan(&act.DeniedSinceOutput); err != nil {
		return act, fmt.Errorf("session activity: denials: %w", err)
	}
	return act, nil
}

// SessionLastEventAt is the time of the session's newest event (MAX(at) over idx_events_session,
// read from the class B log), zero when it has none. An unparsable stamp reads as no activity
// rather than failing the seat card, as before.
func (s *Store) SessionLastEventAt(ctx context.Context, sessionID string) (time.Time, error) {
	b := s.all.B().Reader()
	var at sql.NullString
	if err := b.QueryRowContext(ctx, rebind(b, `SELECT MAX(at) FROM events WHERE session_id = ?`), sessionID).Scan(&at); err != nil {
		return time.Time{}, err
	}
	if !at.Valid || at.String == "" {
		return time.Time{}, nil
	}
	t, _ := time.Parse(time.RFC3339Nano, at.String)
	return t, nil
}

// SessionBootDir returns the path of the session's latest session.boot_dir_planted event
// (sql.ErrNoRows when none, as before). On Postgres the payload is read under the pg_input_is_valid
// guard after each \u0000 escape is replaced with U+FFFD, so the event SQLite would read is read.
func (s *Store) SessionBootDir(ctx context.Context, sessionID string) (string, error) {
	b := s.all.B().Reader()
	path, valid := `json_extract(payload_json,'$.path')`, `json_valid(payload_json)`
	if b.Dialect().Name() == gostore.DriverPostgres {
		const p = `replace(payload_json, '\u0000', '�')`
		valid = `pg_input_is_valid(` + p + `, 'jsonb')`
		path = `(CASE WHEN ` + valid + ` THEN (` + p + `)::jsonb->>'path' END)`
	}
	var root string
	err := b.QueryRowContext(ctx, rebind(b, `SELECT `+path+` FROM events WHERE session_id = ? AND kind = 'session.boot_dir_planted' AND `+valid+`
 ORDER BY id DESC LIMIT 1`), sessionID).Scan(&root)
	return root, err
}

// LatestSessionForAgent returns the agent's newest session (nil when none). Ties on created_at
// (second resolution) break by id DESC; the old store broke them by rowid, insertion order, which
// has no Postgres equivalent.
func (s *Store) LatestSessionForAgent(ctx context.Context, agentID string) (*SessionRow, error) {
	r := s.c.Reader()
	var id string
	err := r.QueryRowContext(ctx, rebind(r, `SELECT id FROM sessions WHERE logical_agent_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`), agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row, err := s.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// AgentHasLiveSession reports whether the agent has a session in a live state.
func (s *Store) AgentHasLiveSession(ctx context.Context, agentID string) (bool, error) {
	r := s.c.Reader()
	var live bool
	err := r.QueryRowContext(ctx, rebind(r, `SELECT EXISTS(SELECT 1 FROM sessions WHERE logical_agent_id = ? AND state IN `+polLiveSessionStates+`)`), agentID).Scan(&live)
	return live, err
}

// ClearNativeResume forgets a provider-native session id: for the source session (or, when the
// source was replaced by sessionID, for sessionID; ErrSessionReplacementUnavailable when it was
// replaced by another) the tether-owned mapping's native id is cleared if it is still nativeID
// (a NULL mapping is inserted when there is none), and sessionID's launch plan drops
// resume_provider_session_id if it is still nativeID. The plan is edited in Go (json_remove in the
// old store) under a lock on its row taken first, so a concurrent plan edit is not lost; the
// edited plan is re-encoded with its keys sorted.
func (s *Store) ClearNativeResume(ctx context.Context, sourceID, sessionID, providerID, nativeID string) error {
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		plan, found, err := polLockPlan(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		var replacement string
		err = tx.QueryRowContext(ctx, rebind(tx, `SELECT replacement_session_id FROM session_replacements WHERE source_session_id = ?`), sourceID).Scan(&replacement)
		switch {
		case err == nil:
			if replacement != sessionID {
				return ErrSessionReplacementUnavailable
			}
			sourceID = sessionID
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		now := s.secondStamp()
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO session_provider_mappings (session_id, owner, provider, native_session_id, created_at, updated_at)
 VALUES (?, 'tether', ?, NULL, ?, ?) ON CONFLICT (session_id, owner, provider) DO UPDATE SET native_session_id = NULL, updated_at = excluded.updated_at
 WHERE session_provider_mappings.native_session_id = ?`), sourceID, providerID, now, now, nativeID); err != nil {
			return err
		}
		if !found {
			return nil
		}
		fields, ok, err := polPlanObject(plan)
		if err != nil || !ok {
			return err
		}
		if !polStringField(fields, "resume_provider_session_id", nativeID) {
			return nil
		}
		delete(fields, "resume_provider_session_id")
		return polWritePlan(ctx, tx, sessionID, fields)
	})
}

// SetNativeStateRoot sets the launch plan's native_state_root (ErrSessionNotFound when the session
// has no plan). The plan is edited in Go (json_set in the old store) under a lock on its row; a
// plan that is not a JSON object is left as it is, as json_set left it.
func (s *Store) SetNativeStateRoot(ctx context.Context, sessionID, root string) error {
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		plan, found, err := polLockPlan(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if !found {
			return ErrSessionNotFound
		}
		fields, ok, err := polPlanObject(plan)
		if err != nil || !ok {
			return err
		}
		raw, err := json.Marshal(root)
		if err != nil {
			return err
		}
		fields["native_state_root"] = raw
		return polWritePlan(ctx, tx, sessionID, fields)
	})
}

// polLockPlan reads the session's launch plan, locking its row on Postgres (SQLite's write
// transaction already holds the one writer).
func polLockPlan(ctx context.Context, tx store.Tx, sessionID string) (plan string, found bool, err error) {
	lock := ""
	if tx.Dialect().Name() == gostore.DriverPostgres {
		lock = " FOR UPDATE"
	}
	err = tx.QueryRowContext(ctx, rebind(tx, `SELECT plan_json FROM launch_plans WHERE session_id = ?`+lock), sessionID).Scan(&plan)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return plan, err == nil, err
}

// polPlanObject decodes a plan's top-level fields; ok is false when the plan is not an object
// (null, as CreateSession stores an absent plan). Invalid JSON is an error, as json_extract and
// json_set raised on it.
func polPlanObject(plan string) (fields map[string]json.RawMessage, ok bool, err error) {
	var v any
	if err := json.Unmarshal([]byte(plan), &v); err != nil {
		return nil, false, fmt.Errorf("launch plan: %w", err)
	}
	if _, isObject := v.(map[string]any); !isObject {
		return nil, false, nil
	}
	if err := json.Unmarshal([]byte(plan), &fields); err != nil {
		return nil, false, fmt.Errorf("launch plan: %w", err)
	}
	return fields, true, nil
}

// polStringField reports whether the field holds the JSON string want (json_extract(...) = ? in the
// old store: a missing or non-string value does not match).
func polStringField(fields map[string]json.RawMessage, key, want string) bool {
	var got string
	raw, has := fields[key]
	if !has {
		return false
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	return got == want
}

func polWritePlan(ctx context.Context, tx store.Tx, sessionID string, fields map[string]json.RawMessage) error {
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, rebind(tx, `UPDATE launch_plans SET plan_json = ? WHERE session_id = ?`), string(raw), sessionID)
	return err
}
