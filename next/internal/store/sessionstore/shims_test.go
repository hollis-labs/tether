package sessionstore_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/sessionstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

func shimRow(id string) sessionstore.SessionShimRow {
	return sessionstore.SessionShimRow{SessionID: id, ShimKey: "key-" + id, HostBackend: "detached", SocketPath: "/run/" + id + ".sock",
		DescriptorPath: "/run/" + id + "/launch.json", Runtime: "codex-app-server", RuntimeGeneration: 1, BootGeneration: "boot-1"}
}

func TestShimsPlacement(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		if err := ss.UpsertSessionShim(ctx, shimRow("nope")); !errors.Is(err, sessionstore.ErrSessionNotFound) {
			t.Fatalf("missing session: %v", err)
		}
		repRunning(t, s, ss, "ses-1", 42)
		first := shimRow("ses-1")
		if err := ss.UpsertSessionShim(ctx, first); err != nil {
			t.Fatal(err)
		}
		stored, err := ss.SessionShim(ctx, "ses-1")
		if err != nil || stored.CreatedAt == "" || stored.ShimKey != "key-ses-1" {
			t.Fatalf("stored %+v %v", stored, err)
		}
		bad := first
		bad.SocketPath = "relative"
		if err := ss.UpsertSessionShim(ctx, bad); err == nil {
			t.Fatal("relative socket accepted")
		}
		moved := first
		moved.SocketPath = "/run/other.sock"
		if err := ss.UpsertSessionShim(ctx, moved); !errors.Is(err, sessionstore.ErrSessionShimConflict) {
			t.Fatalf("identity change: %v", err)
		}
		adv := first
		adv.JournalID, adv.LastCommittedCursor, adv.InjectCounter, adv.ControllerEpoch, adv.HostPID = "j1", "j1:5", 3, 2, 100
		if err := ss.UpsertSessionShim(ctx, adv); err != nil {
			t.Fatal(err)
		}
		got, _ := ss.SessionShim(ctx, "ses-1")
		if got.InjectCounter != 3 || got.JournalID != "j1" || got.HostPID != 100 || got.CreatedAt != stored.CreatedAt {
			t.Fatalf("advanced %+v", got)
		}
		for name, mut := range map[string]func(*sessionstore.SessionShimRow){
			"counter back":   func(r *sessionstore.SessionShimRow) { r.InjectCounter = 2 },
			"epoch back":     func(r *sessionstore.SessionShimRow) { r.ControllerEpoch = 1 },
			"cursor back":    func(r *sessionstore.SessionShimRow) { r.LastCommittedCursor = "j1:4" },
			"journal change": func(r *sessionstore.SessionShimRow) { r.JournalID, r.LastCommittedCursor = "j2", "" },
		} {
			r := adv
			mut(&r)
			if err := ss.UpsertSessionShim(ctx, r); !errors.Is(err, sessionstore.ErrSessionShimConflict) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if list, _ := ss.ListSessionShims(ctx); len(list) != 1 {
			t.Fatalf("list %+v", list)
		}
		if err := ss.RemoveUnstartedSessionShim(ctx, "ses-1", "key-ses-1"); !errors.Is(err, sessionstore.ErrSessionShimConflict) {
			t.Fatalf("remove with a host pid: %v", err)
		}
		if _, _, err := ss.CompleteSessionShim(ctx, "ses-1", "running", 0); err == nil {
			t.Fatal("non-terminal state accepted")
		}
		change, done, err := ss.CompleteSessionShim(ctx, "ses-1", "failed", 3)
		if err != nil || !done || change.From != "running" || change.LogicalAgentID != "agent" {
			t.Fatalf("complete %+v %v %v", change, done, err)
		}
		if _, done, _ := ss.CompleteSessionShim(ctx, "ses-1", "killed", 9); done {
			t.Fatal("a second terminal outcome replaced the first")
		}
		if st := repState(t, ss, "ses-1"); st.State != "failed" || st.ExitCode == nil || *st.ExitCode != 3 {
			t.Fatalf("terminal %+v", st)
		}
		repRunning(t, s, ss, "ses-2", 7)
		if err := ss.UpsertSessionShim(ctx, shimRow("ses-2")); err != nil {
			t.Fatal(err)
		}
		if err := ss.RemoveUnstartedSessionShim(ctx, "ses-2", "key-ses-2"); err != nil {
			t.Fatal(err)
		}
		if _, err := ss.SessionShim(ctx, "ses-2"); !errors.Is(err, sessionstore.ErrSessionShimNotFound) {
			t.Fatalf("removed shim: %v", err)
		}
	})
}

// TestShimsConcurrent: racing upserts never leave the stored inject counter below one a racer
// committed (the monotonic check and the write run under the session row lock), and exactly one of
// racing terminal outcomes wins.
func TestShimsConcurrent(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		repRunning(t, s, ss, "ses-1", 42)
		if err := ss.UpsertSessionShim(ctx, shimRow("ses-1")); err != nil {
			t.Fatal(err)
		}
		repWarm(s, 6)
		var counter int64
		counters, errs := repRace(6, func() (uint64, error) {
			r := shimRow("ses-1")
			r.InjectCounter = uint64(atomicAdd(&counter))
			return r.InjectCounter, ss.UpsertSessionShim(ctx, r)
		})
		var maxOK uint64
		for i, c := range counters {
			if errs[i] == nil && c > maxOK {
				maxOK = c
			} else if errs[i] != nil && !errors.Is(errs[i], sessionstore.ErrSessionShimConflict) {
				t.Fatalf("upsert: %v", errs[i])
			}
		}
		if got, _ := ss.SessionShim(ctx, "ses-1"); got.InjectCounter != maxOK {
			t.Fatalf("stored inject counter %d, highest committed %d", got.InjectCounter, maxOK)
		}
		repWarm(s, 6)
		var n int64
		wins, errs := repRace(6, func() (bool, error) {
			state := []string{"completed", "failed", "killed"}[atomicAdd(&n)%3]
			_, done, err := ss.CompleteSessionShim(ctx, "ses-1", state, 0)
			return done, err
		})
		won := 0
		for i, w := range wins {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}
			if w {
				won++
			}
		}
		if won != 1 {
			t.Fatalf("%d terminal outcomes won, want 1", won)
		}
	})
}

func shimRecord(op, session, revision, phase string) sessionstore.ShimRetirementRecord {
	return sessionstore.ShimRetirementRecord{OperationID: op, SessionID: session, RequestDigest: "rd", Revision: revision, Phase: phase,
		RecordJSON: fmt.Sprintf(`{"operation_id":%q,"revision":%q}`, op, revision)}
}

func TestShimRetirement(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		if err := ss.RecordShimRetirement(ctx, shimRecord("op-1", "nope", "r1", "intent_recorded"), ""); !errors.Is(err, sessionstore.ErrShimRetirementConflict) {
			t.Fatalf("missing session: %v", err)
		}
		repRunning(t, s, ss, "ses-1", 42)
		if _, err := ss.ShimRetirement(ctx, "op-1"); !errors.Is(err, sessionstore.ErrShimRecordNotFound) {
			t.Fatalf("missing record: %v", err)
		}
		if err := ss.RecordShimRetirement(ctx, shimRecord("op-1", "ses-1", "r1", "intent_recorded"), ""); err != nil {
			t.Fatal(err)
		}
		if err := ss.RecordShimRetirement(ctx, shimRecord("op-1", "ses-1", "r1b", "intent_recorded"), ""); !errors.Is(err, sessionstore.ErrShimRetirementConflict) {
			t.Fatalf("second first record: %v", err)
		}
		if err := ss.RecordShimRetirement(ctx, shimRecord("op-1", "ses-1", "r2", "retirement_committed"), "stale"); !errors.Is(err, sessionstore.ErrShimRetirementConflict) {
			t.Fatalf("stale revision: %v", err)
		}
		cleanup := shimRecord("op-1", "ses-1", "r2", "descriptor_cleanup_complete")
		cleanup.RetiredAt = "2026-11-14T12:00:00Z"
		if err := ss.RecordShimRetirement(ctx, cleanup, "r1"); err != nil {
			t.Fatal(err)
		}
		got, err := ss.ShimRetirement(ctx, "op-1")
		if err != nil || got.Revision != "r2" || got.Phase != "descriptor_cleanup_complete" || got.RetiredAt != cleanup.RetiredAt || got.RecordJSON != cleanup.RecordJSON {
			t.Fatalf("record %+v %v", got, err)
		}
		if rev, err := sessionstore.NewShimRevision(); err != nil || len(rev) != 64 {
			t.Fatalf("revision %q %v", rev, err)
		}

		// Reconcile: the policy's check binds the placement; the shim row must match it.
		if err := ss.UpsertSessionShim(ctx, shimRow("ses-1")); err != nil {
			t.Fatal(err)
		}
		want := sessionstore.ShimPlacementExpect{ShimKey: "key-ses-1", Generation: 1, HostBackend: "detached"}
		bound := func(rec sessionstore.ShimRetirementRecord) (sessionstore.ShimPlacementExpect, error) {
			if rec.Phase != "descriptor_cleanup_complete" {
				return want, errors.New("not cleaned up")
			}
			return want, nil
		}
		other := want
		other.Generation = 2
		if _, err := ss.ReconcileShimRetirement(ctx, "ses-1", "op-1", func(sessionstore.ShimRetirementRecord) (sessionstore.ShimPlacementExpect, error) {
			return other, nil
		}); !errors.Is(err, sessionstore.ErrShimRetirementConflict) {
			t.Fatalf("other placement: %v", err)
		}
		if p, b := repRevoked(t, s, "ses-1"); p || b {
			t.Fatal("a refused reconcile revoked authority")
		}
		change, err := ss.ReconcileShimRetirement(ctx, "ses-1", "op-1", bound)
		if err != nil || change.From != "running" || change.To != "orphaned" {
			t.Fatalf("reconcile %+v %v", change, err)
		}
		if st := repState(t, ss, "ses-1"); st.State != "orphaned" || st.PID != nil || st.EndedAt != "" {
			t.Fatalf("reconciled session %+v", st)
		}
		if p, b := repRevoked(t, s, "ses-1"); !p || !b {
			t.Fatalf("revoked principal %v binding %v", p, b)
		}

		// Retention cursors.
		c := sessionstore.ShimRetentionCursor{CursorID: "cur-1", RequestDigest: "rd", Revision: "c1", RecordJSON: `{"id":"cur-1"}`}
		if err := ss.RecordShimRetentionCursor(ctx, c, ""); err != nil {
			t.Fatal(err)
		}
		next := c
		next.Revision, next.RecordJSON = "c2", `{"id":"cur-1","last":"a"}`
		if err := ss.RecordShimRetentionCursor(ctx, next, "c0"); !errors.Is(err, sessionstore.ErrShimRetirementConflict) {
			t.Fatalf("stale cursor: %v", err)
		}
		if err := ss.RecordShimRetentionCursor(ctx, next, "c1"); err != nil {
			t.Fatal(err)
		}
		if got, err := ss.ShimRetentionCursor(ctx, "cur-1"); err != nil || got.Revision != "c2" || got.RecordJSON != next.RecordJSON {
			t.Fatalf("cursor %+v %v", got, err)
		}
		if _, err := ss.ShimRetentionCursor(ctx, "nope"); !errors.Is(err, sessionstore.ErrShimRecordNotFound) {
			t.Fatalf("missing cursor: %v", err)
		}
	})
}

// TestShimRetirementConcurrent: racing writes from one revision: exactly one applies.
func TestShimRetirementConcurrent(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		repRunning(t, s, ss, "ses-1", 42)
		if err := ss.RecordShimRetirement(ctx, shimRecord("op-1", "ses-1", "r1", "intent_recorded"), ""); err != nil {
			t.Fatal(err)
		}
		repWarm(s, 6)
		var n int64
		_, errs := repRace(6, func() (struct{}, error) {
			rev := "r2-" + strconv.FormatInt(atomicAdd(&n), 10)
			return struct{}{}, ss.RecordShimRetirement(ctx, shimRecord("op-1", "ses-1", rev, "retirement_committed"), "r1")
		})
		ok := 0
		for _, err := range errs {
			if err == nil {
				ok++
			} else if !errors.Is(err, sessionstore.ErrShimRetirementConflict) {
				t.Fatal(err)
			}
		}
		if ok != 1 {
			t.Fatalf("%d writes applied from one revision, want 1", ok)
		}
	})
}

func TestShimTeamRecovery(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		repTeamSource(t, s, ss)
		// A live team member (repTeamSource leaves its source terminal for replacement).
		repExec(t, s.C(), `UPDATE sessions SET state = 'running', ended_at = NULL, exit_code = NULL WHERE id = 'ses-src'`)
		custody := shimRow("ses-src")
		custody.HostPID = 11
		if err := ss.UpsertSessionShim(ctx, custody); err != nil {
			t.Fatal(err)
		}
		custody, _ = ss.SessionShim(ctx, "ses-src")
		stale := custody
		stale.HostPID = 12
		if _, err := ss.GoneTeamShimRecoveryFence(ctx, stale); !errors.Is(err, sessionstore.ErrTeamRecoveryUnavailable) {
			t.Fatalf("stale custody: %v", err)
		}
		f, err := ss.GoneTeamShimRecoveryFence(ctx, custody)
		if err != nil || f.BindingID != "bind-1" || f.IntentKey != "ik-1" || f.Custody != custody {
			t.Fatalf("fence %+v %v", f, err)
		}
		changed := f
		changed.BindingGeneration++
		if _, err := ss.CommitGoneTeamShimRecovery(ctx, changed); !errors.Is(err, sessionstore.ErrTeamRecoveryUnavailable) {
			t.Fatalf("changed fence: %v", err)
		}
		change, err := ss.CommitGoneTeamShimRecovery(ctx, f)
		if err != nil || change.From != "running" || change.Reason != "team_shim_recovery_pending" {
			t.Fatalf("commit %+v %v", change, err)
		}
		if _, err := ss.SessionShim(ctx, "ses-src"); !errors.Is(err, sessionstore.ErrSessionShimNotFound) {
			t.Fatalf("custody row kept: %v", err)
		}
		if st := repState(t, ss, "ses-src"); st.State != "orphaned" {
			t.Fatalf("state %q", st.State)
		}
		if got := repText(t, s.C().Reader(), `SELECT state FROM team_gone_shim_recoveries WHERE session_id = 'ses-src'`); got != "recovery_pending" {
			t.Fatalf("archive %q", got)
		}
		if _, err := ss.CommitGoneTeamShimRecovery(ctx, f); !errors.Is(err, sessionstore.ErrTeamRecoveryUnavailable) {
			t.Fatalf("second commit: %v", err)
		}
	})
}

func TestShimCodexProtocol(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		repRunning(t, s, ss, "ses-1", 42)
		if err := ss.UpsertSessionShim(ctx, shimRow("ses-1")); err != nil {
			t.Fatal(err)
		}
		if _, err := ss.CodexProtocol(ctx, "ses-1", "key-ses-1", 1<<20); !errors.Is(err, sessionstore.ErrCodexProtocolMissing) {
			t.Fatalf("missing: %v", err)
		}
		w := sessionstore.CodexProtocolWrite{SessionID: "ses-1", ShimKey: "key-ses-1", Protocol: "v1", Generation: 1, Previous: 0, Next: 1, StateJSON: `{"revision":1}`, MaxBytes: 1 << 20}
		wrongGen := w
		wrongGen.Generation = 2
		if err := ss.CommitCodexProtocol(ctx, wrongGen); !errors.Is(err, sessionstore.ErrSessionShimConflict) {
			t.Fatalf("wrong generation: %v", err)
		}
		if err := ss.CommitCodexProtocol(ctx, w); err != nil {
			t.Fatal(err)
		}
		got, err := ss.CodexProtocol(ctx, "ses-1", "key-ses-1", 1<<20)
		if err != nil || got.Revision != "1" || got.StateJSON != w.StateJSON || got.Protocol != "v1" {
			t.Fatalf("load %+v %v", got, err)
		}
		if _, err := ss.CodexProtocol(ctx, "ses-1", "key-ses-1", 4); !errors.Is(err, sessionstore.ErrSessionShimConflict) {
			t.Fatalf("oversized: %v", err)
		}
		if _, err := ss.CodexProtocol(ctx, "ses-1", "other", 1<<20); !errors.Is(err, sessionstore.ErrSessionShimConflict) {
			t.Fatalf("other key: %v", err)
		}
		next := w
		next.Previous, next.Next, next.StateJSON = 1, 2, `{"revision":2}`
		if err := ss.CommitCodexProtocol(ctx, next); err != nil {
			t.Fatal(err)
		}
		if err := ss.CommitCodexProtocol(ctx, next); !errors.Is(err, sessionstore.ErrSessionShimConflict) {
			t.Fatalf("replayed transition: %v", err)
		}

		// The delivery commit composes the candidate read and its write in one class C transaction.
		err = s.C().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
			c, err := sessionstore.CodexCandidateTx(ctx, tx, "ses-1", sessionstore.CodexCandidateRead{MaxBytes: 1 << 20, RefuseReplaced: true, ForUpdate: true})
			if err != nil {
				return err
			}
			if c.Protocol.Revision != "2" || c.SessionState != "running" || c.Placement.ShimKey != "key-ses-1" {
				return fmt.Errorf("candidate %+v", c)
			}
			return sessionstore.CommitCodexDeliveryTx(ctx, tx, "ses-1", "key-ses-1", 2, 3, `{"revision":3,"delivery":{}}`, 1<<20)
		})
		if err != nil {
			t.Fatal(err)
		}
		if c, err := ss.CodexCandidate(ctx, "ses-1", sessionstore.CodexCandidateRead{MaxBytes: 1 << 20, RefuseReplaced: true, ForUpdate: true}); err != nil || c.Protocol.Revision != "3" {
			t.Fatalf("candidate after delivery %+v %v", c, err)
		}

		// A replaced session's custody is frozen.
		repExec(t, s.C(), `INSERT INTO session_replacements (source_session_id, replacement_session_id, actor_uri, intent_key, binding_id,
 binding_generation, source_plan_digest, credential_scopes_json, credential_mode, committed_at)
 VALUES ('ses-1', 'ses-new', 'msg://agent/x/a', 'ik', 'b', 1, 'd', '[]', 'off', '2026-11-14T00:00:00Z')`)
		if _, err := ss.CodexProtocol(ctx, "ses-1", "key-ses-1", 1<<20); !errors.Is(err, sessionstore.ErrSessionShimConflict) {
			t.Fatalf("replaced load: %v", err)
		}
		if _, err := ss.CodexCandidate(ctx, "ses-1", sessionstore.CodexCandidateRead{MaxBytes: 1 << 20, RefuseReplaced: true}); !errors.Is(err, sessionstore.ErrSessionShimConflict) {
			t.Fatalf("replaced candidate: %v", err)
		}
		if c, err := ss.CodexCandidate(ctx, "ses-1", sessionstore.CodexCandidateRead{MaxBytes: 1 << 20}); err != nil || c.Protocol.Revision != "3" {
			t.Fatalf("historical read of a replaced session: %+v %v", c, err)
		}
		last := next
		last.Previous, last.Next = 3, 4
		if err := ss.CommitCodexProtocol(ctx, last); !errors.Is(err, sessionstore.ErrSessionShimConflict) {
			t.Fatalf("replaced write: %v", err)
		}
	})
}

// TestShimCodexConcurrent: racing protocol transitions from one revision: exactly one applies.
func TestShimCodexConcurrent(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		repRunning(t, s, ss, "ses-1", 42)
		if err := ss.UpsertSessionShim(ctx, shimRow("ses-1")); err != nil {
			t.Fatal(err)
		}
		w := sessionstore.CodexProtocolWrite{SessionID: "ses-1", ShimKey: "key-ses-1", Protocol: "v1", Generation: 1, Next: 1, StateJSON: `{"r":1}`, MaxBytes: 1 << 20}
		if err := ss.CommitCodexProtocol(ctx, w); err != nil {
			t.Fatal(err)
		}
		repWarm(s, 6)
		var n int64
		_, errs := repRace(6, func() (struct{}, error) {
			next := w
			next.Previous, next.Next, next.StateJSON = 1, 2, fmt.Sprintf(`{"r":2,"by":%d}`, atomicAdd(&n))
			return struct{}{}, ss.CommitCodexProtocol(ctx, next)
		})
		ok := 0
		for _, err := range errs {
			if err == nil {
				ok++
			} else if !errors.Is(err, sessionstore.ErrSessionShimConflict) {
				t.Fatal(err)
			}
		}
		if ok != 1 {
			t.Fatalf("%d transitions applied from one revision, want 1", ok)
		}
	})
}

func TestShimCodexLedger(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		repRunning(t, s, ss, "ses-1", 42)
		repExec(t, s.C(), `UPDATE launch_plans SET plan_json = ? WHERE session_id = 'ses-1'`, `{"provider_id":"codex-app-server","provider_brand":"codex","note":"a\u0000b"}`)
		if id, brand, err := ss.CodexPlanProvider(ctx, "ses-1", 1<<20); err != nil || id != "codex-app-server" || brand != "codex" {
			t.Fatalf("plan provider %q %q %v", id, brand, err)
		}
		if id, _, err := ss.CodexPlanProvider(ctx, "ses-1", 8); err == nil || id != "" {
			t.Fatalf("oversized plan read: %q %v", id, err)
		}
		r := s.C().Reader()
		before, err := sessionstore.CodexPublicLedgerDigest(ctx, r, "ses-1")
		if err != nil {
			t.Fatal(err)
		}
		repExec(t, s.B(), `INSERT INTO events (scope, session_id, kind, payload_json, at, recorded_at, event_id, pending)
 VALUES ('session', 'ses-1', 'session.turn_output', ?, '2026-11-14T00:00:00Z', '2026-11-14T00:00:00Z', 'evt-1', 0)`, `{"output_id":"out-1","note":"a\u0000b"}`)
		repExec(t, s.C(), `INSERT INTO messages (id, kind, channel, from_urn, to_urn, payload, content_type, metadata, created_at, routing_staged)
 VALUES ('m-1', 'notice', '', 'msg://session/local/ses-1', 'msg://agent/x/y', '{}', 'application/json', '{"output_id":"out-1"}', '2026-11-14T00:00:00Z', 0)`)
		repExec(t, s.C(), `INSERT INTO messages (id, kind, channel, from_urn, to_urn, payload, content_type, metadata, created_at, routing_staged)
 VALUES ('m-2', 'notice', '', 'msg://session/local/ses-1', 'msg://agent/x/y', '{}', 'application/json', '{"output_id":null}', '2026-11-14T00:00:01Z', 0)`)
		after, err := sessionstore.CodexPublicLedgerDigest(ctx, r, "ses-1")
		if err != nil || after == before {
			t.Fatalf("digest %q -> %q %v", before, after, err)
		}
		if again, _ := sessionstore.CodexPublicLedgerDigest(ctx, r, "ses-1"); again != after {
			t.Fatal("digest is not stable")
		}
		// m-2's output_id is JSON null: not an outbox message, on either dialect.
		if outputs, messages, err := sessionstore.CodexPublicCounts(ctx, r, "ses-1"); err != nil || outputs != 1 || messages != 1 {
			t.Fatalf("counts %d %d %v", outputs, messages, err)
		}
		if payload, found, err := sessionstore.CodexTurnOutputPayload(ctx, r, "ses-1", "out-1"); err != nil || !found || payload == "" {
			t.Fatalf("turn output %q %v %v", payload, found, err)
		}
		if _, found, err := sessionstore.CodexTurnOutputPayload(ctx, r, "ses-1", "out-2"); err != nil || found {
			t.Fatalf("missing turn output %v %v", found, err)
		}
		if m, found, err := sessionstore.CodexOutputMessageTx(ctx, r, "m-1"); err != nil || !found || m.Staged || m.Published {
			t.Fatalf("output message %+v %v %v", m, found, err)
		}
		if _, _, _, err := sessionstore.CodexMessagingLedger(ctx, r, "ses-1"); s.C().Dialect().Name() == gostore.DriverPostgres {
			if !errors.Is(err, store.ErrLibraryUnavailable) {
				t.Fatalf("postgres messaging ledger: %v", err)
			}
		} else if err == nil {
			// go-messaging's tables are created by the library (not in this test's schema); without
			// them the SQLite read fails rather than reporting an empty ledger.
			t.Fatal("messaging ledger read without the library's tables")
		}
	})
}

func atomicAdd(p *int64) int64 { return atomic.AddInt64(p, 1) }

// TestShimTeamRecoveryConcurrent: racing commits of one fence: exactly one archives the custody,
// the rest see it gone (ErrTeamRecoveryUnavailable), never a duplicate-key error. The hold keeps
// each racer that passed every check until all have (or 300ms pass): with the session row lock only
// one racer ever reaches it at a time; without the lock all do, and all go on to write.
func TestShimTeamRecoveryConcurrent(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		repTeamSource(t, s, ss)
		repExec(t, s.C(), `UPDATE sessions SET state = 'running', ended_at = NULL, exit_code = NULL WHERE id = 'ses-src'`)
		if err := ss.UpsertSessionShim(ctx, shimRow("ses-src")); err != nil {
			t.Fatal(err)
		}
		custody, _ := ss.SessionShim(ctx, "ses-src")
		f, err := ss.GoneTeamShimRecoveryFence(ctx, custody)
		if err != nil {
			t.Fatal(err)
		}
		const racers = 6
		var arrived atomic.Int32
		var open sync.Once
		gate := make(chan struct{})
		defer sessionstore.SetTeamCommitHold(func() {
			if arrived.Add(1) >= racers {
				open.Do(func() { close(gate) })
			}
			select {
			case <-gate:
			case <-time.After(300 * time.Millisecond):
			}
		})()
		repWarm(s, racers)
		_, errs := repRace(racers, func() (sessionstore.SessionStateChange, error) { return ss.CommitGoneTeamShimRecovery(ctx, f) })
		ok := 0
		for _, err := range errs {
			if err == nil {
				ok++
			} else if !errors.Is(err, sessionstore.ErrTeamRecoveryUnavailable) {
				t.Fatalf("commit: %v", err)
			}
		}
		if ok != 1 {
			t.Fatalf("%d commits applied, want 1", ok)
		}
	})
}
