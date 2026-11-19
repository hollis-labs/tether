package sessionstore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
	"github.com/hollis-labs/tether/next/internal/store/sessionstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

func polExec(t *testing.T, h *store.Handle, q string, args ...any) {
	t.Helper()
	if err := h.WriteTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, tx.Dialect().Rebind(q), args...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// polRace runs fn on n goroutines at once and returns their results.
func polRace[T any](n int, fn func() (T, error)) ([]T, []error) {
	out, errs := make([]T, n), make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out[i], errs[i] = fn()
		}()
	}
	close(start)
	wg.Wait()
	return out, errs
}

// polWarm opens enough pool connections that racers overlap instead of staggering on connection
// setup.
func polWarm(ctx context.Context, s *store.Stores) {
	polRace(6, func() (struct{}, error) {
		return struct{}{}, s.C().WriteTx(ctx, func(context.Context, store.Tx) error { time.Sleep(50 * time.Millisecond); return nil })
	})
}

func polCreate(t *testing.T, ss *sessionstore.Store, r sessionstore.SessionRow, plan string) {
	t.Helper()
	if _, err := ss.CreateSession(context.Background(), r, plan, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyMCP(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		live := row("ses-live")
		live.State, live.ProviderKind = "running", "cli"
		polCreate(t, ss, live, "")
		done := row("ses-done")
		done.State = "completed"
		polCreate(t, ss, done, "")
		api := row("ses-api")
		api.State, api.ProviderKind = "running", "api"
		polCreate(t, ss, api, "")

		if _, err := ss.SaveSessionMCPPolicy(ctx, "ses-live", "other-agent", `{"digest":"x"}`); !errors.Is(err, sessionstore.ErrSessionMCPPolicyUnavailable) {
			t.Fatalf("another agent's session: %v", err)
		}
		if _, err := ss.SaveSessionMCPPolicy(ctx, "ses-done", "agent", `{"digest":"x"}`); !errors.Is(err, sessionstore.ErrSessionMCPPolicyUnavailable) {
			t.Fatalf("terminal session: %v", err)
		}
		// Concurrent saves of different policies: one binds, and every caller reads the bound one.
		polWarm(ctx, s)
		var n int
		var mu sync.Mutex
		stored, errs := polRace(6, func() (string, error) {
			mu.Lock()
			n++
			digest := strings.Repeat("d", n)
			mu.Unlock()
			return ss.SaveSessionMCPPolicy(ctx, "ses-live", "agent", `{"digest":"`+digest+`","upstream_ownership":"daemon"}`)
		})
		for i := range stored {
			if errs[i] != nil || stored[i] != stored[0] {
				t.Fatalf("concurrent saves: %v %q vs %q", errs[i], stored[i], stored[0])
			}
		}
		if policy, agent, err := ss.SessionMCPPolicyJSON(ctx, "ses-live"); err != nil || policy != stored[0] || agent != "agent" {
			t.Fatalf("policy %q %q %v", policy, agent, err)
		}
		if _, _, err := ss.SessionMCPPolicyJSON(ctx, "ses-done"); !errors.Is(err, sessionstore.ErrSessionMCPPolicyUnavailable) {
			t.Fatalf("no policy: %v", err)
		}
		counts, err := ss.MCPUpstreamSessionCounts(ctx)
		if err != nil || !reflect.DeepEqual(counts, map[string]int{"legacy_proxy": 0, "daemon": 1, "unknown": 0}) {
			t.Fatalf("counts %v %v", counts, err)
		}

		// Recovery needs an active session credential and a binding at the target's latest generation.
		if _, _, err := ss.RecoverySessionMCPPolicyJSON(ctx, "ses-live"); !errors.Is(err, sessionstore.ErrSessionMCPPolicyUnavailable) {
			t.Fatalf("recovery without credential: %v", err)
		}
		future := clock.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
		past := clock.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
		polExec(t, s.A(), `INSERT INTO principals (principal_id, kind, display, token_hash, scopes_json, session_id, addresses_json, created_at, expires_at)
 VALUES ('msg://session/local/ses-live', 'session', 'd', ?, '[]', 'ses-live', '[]', ?, ?)`, strings.Repeat("a", 64), past, future)
		if _, _, err := ss.RecoverySessionMCPPolicyJSON(ctx, "ses-live"); !errors.Is(err, sessionstore.ErrSessionMCPPolicyUnavailable) {
			t.Fatalf("recovery without binding: %v", err)
		}
		bind := func(id, session string, gen int, expires any) {
			polExec(t, s.C(), `INSERT INTO runtime_bindings (id, target_urn, session_id, host_id, attempt_id, generation, leased_at, lease_expires_at, created_at, updated_at)
 VALUES (?, 'msg://agent/x/a', ?, 'h', 'att', ?, ?, ?, ?, ?)`, id, session, gen, past, expires, past, past)
		}
		bind("b1", "ses-live", 1, future)
		if policy, _, err := ss.RecoverySessionMCPPolicyJSON(ctx, "ses-live"); err != nil || policy != stored[0] {
			t.Fatalf("recovery %q %v", policy, err)
		}
		bind("b2", "ses-other", 2, nil) // a newer generation for the target belongs to another session
		if _, _, err := ss.RecoverySessionMCPPolicyJSON(ctx, "ses-live"); !errors.Is(err, sessionstore.ErrSessionMCPPolicyUnavailable) {
			t.Fatalf("recovery with a superseded binding: %v", err)
		}
	})
}

func TestPolicyWorkerParserAndActivity(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		if st, err := ss.WorkerPermissionParser(ctx, "ses-1"); err != nil || st.Spans != "[]" || st.Sequence != 0 {
			t.Fatalf("empty parser %+v %v", st, err)
		}
		_ = ss.SaveWorkerPermissionParser(ctx, "ses-1", sessionstore.WorkerPermissionParser{Sequence: 5, Spans: `[1]`, Discard: true})
		_ = ss.SaveWorkerPermissionParser(ctx, "ses-1", sessionstore.WorkerPermissionParser{Sequence: 3, Spans: `[old]`})
		if st, _ := ss.WorkerPermissionParser(ctx, "ses-1"); st.Sequence != 5 || st.Spans != `[1]` || !st.Discard {
			t.Fatalf("parser %+v", st)
		}

		es := eventstore.New(s)
		es.Now = clock.Now
		if act, err := ss.SessionActivity(ctx, "ses-1"); err != nil || !act.LastEventAt.IsZero() || act.DeniedSinceOutput != 0 {
			t.Fatalf("no events %+v %v", act, err)
		}
		if _, err := ss.SessionBootDir(ctx, "ses-1"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("no boot dir: %v", err)
		}
		if at, err := ss.SessionLastEventAt(ctx, "ses-1"); err != nil || !at.IsZero() {
			t.Fatalf("last event with none: %v %v", at, err)
		}
		insert := func(kind, payload string) {
			t.Helper()
			if _, err := es.Insert(ctx, eventstore.Row{Scope: "session", SessionID: "ses-1", Kind: kind, PayloadJSON: payload, At: clock.Now()}); err != nil {
				t.Fatal(err)
			}
			clock.Advance(time.Second)
		}
		insert("provider.permission_denied", "")
		insert("session.turn_output", "")
		insert("provider.permission_denied", "")
		insert("provider.permission_denied", "")
		insert("session.boot_dir_planted", `{"path":"/boot/one"}`)
		insert("session.boot_dir_planted", `{"path":"/boot/two","note":"a\u0000b"}`) // jsonb refuses \u0000; still read
		insert("session.boot_dir_planted", `not json`)
		act, err := ss.SessionActivity(ctx, "ses-1")
		if err != nil || act.DeniedSinceOutput != 2 || !act.LastEventAt.Equal(clock.Now().Add(-time.Second)) {
			t.Fatalf("activity %+v %v", act, err)
		}
		if at, err := ss.SessionLastEventAt(ctx, "ses-1"); err != nil || !at.Equal(act.LastEventAt) {
			t.Fatalf("last event %v %v, want %v", at, err, act.LastEventAt)
		}
		if dir, err := ss.SessionBootDir(ctx, "ses-1"); err != nil || dir != "/boot/two" {
			t.Fatalf("boot dir %q %v", dir, err)
		}
		// Only an unescaped \u0000 is mapped: a literal backslash before "u0000" in the path is kept
		// as it is, and adjacent escapes elsewhere in the payload are all mapped (two passes).
		insert("session.boot_dir_planted", `{"path":"/b\\u0000x"}`)
		if dir, err := ss.SessionBootDir(ctx, "ses-1"); err != nil || dir != `/b\u0000x` {
			t.Fatalf("boot dir with a literal backslash-u0000 %q %v", dir, err)
		}
		insert("session.boot_dir_planted", `{"path":"/boot/three","n":"\u0000\u0000\u0000"}`)
		if dir, err := ss.SessionBootDir(ctx, "ses-1"); err != nil || dir != "/boot/three" {
			t.Fatalf("boot dir after adjacent escapes %q %v", dir, err)
		}
	})
}

func TestPolicyResumeAndPlan(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		if got, err := ss.LatestSessionForAgent(ctx, "agent"); err != nil || got != nil {
			t.Fatalf("no session %+v %v", got, err)
		}
		a := row("ses-a")
		a.State = "completed"
		polCreate(t, ss, a, `{"resume_provider_session_id":"n1","keep":[1,2]}`)
		b := row("ses-b") // same created_at second as ses-a: id breaks the tie
		polCreate(t, ss, b, "")
		if got, _ := ss.LatestSessionForAgent(ctx, "agent"); got == nil || got.ID != "ses-b" {
			t.Fatalf("latest %+v", got)
		}
		clock.Advance(time.Second)
		// Same-second ties: SQLite keeps the old rowid (insertion) order; Postgres breaks them by id.
		y, c := row("ses-y"), row("ses-c")
		y.LogicalAgentID, c.LogicalAgentID = "agent2", "agent2"
		polCreate(t, ss, y, "")
		polCreate(t, ss, c, "")
		want := "ses-c" // inserted last
		if s.C().Dialect().Name() == gostore.DriverPostgres {
			want = "ses-y" // id DESC
		}
		if got, _ := ss.LatestSessionForAgent(ctx, "agent2"); got == nil || got.ID != want {
			t.Fatalf("same-second tie %+v, want %s", got, want)
		}
		if id, _ := ss.LatestSessionForLogicalAgent(ctx, "agent2"); id != want {
			t.Fatalf("same-second tie (logical) %q, want %s", id, want)
		}
		if live, _ := ss.AgentHasLiveSession(ctx, "agent"); !live {
			t.Fatal("launching session not live")
		}
		if live, _ := ss.AgentHasLiveSession(ctx, "nobody"); live {
			t.Fatal("unknown agent live")
		}

		if err := ss.SetNativeStateRoot(ctx, "nope", "/r"); !errors.Is(err, sessionstore.ErrSessionNotFound) {
			t.Fatalf("missing plan: %v", err)
		}
		if err := ss.SetNativeStateRoot(ctx, "ses-b", "/r"); err != nil { // a null plan is left as it is
			t.Fatal(err)
		}
		if plan, _ := ss.LaunchPlanJSON(ctx, "ses-b"); plan != "null" {
			t.Fatalf("null plan %q", plan)
		}
		_ = ss.UpsertSessionProviderMapping(ctx, "ses-a", "tether", "claude", "n1")
		if err := ss.ClearNativeResume(ctx, "ses-a", "ses-a", "claude", "other"); err != nil {
			t.Fatal(err)
		}
		if m, _ := ss.GetSessionProviderMapping(ctx, "ses-a", "tether", "claude"); m.NativeSessionID != "n1" {
			t.Fatalf("a different native id cleared the mapping: %+v", m)
		}

		// A replaced source resolves to its replacement; a third session may not clear it.
		polExec(t, s.C(), `INSERT INTO session_replacements (source_session_id, replacement_session_id, actor_uri, intent_key, binding_id, binding_generation,
 source_plan_digest, credential_scopes_json, credential_mode, committed_at) VALUES ('ses-old', 'ses-a', 'u', 'k', 'b', 1, 'd', '[]', 'off', 't')`)
		if err := ss.ClearNativeResume(ctx, "ses-old", "ses-b", "claude", "n1"); !errors.Is(err, sessionstore.ErrSessionReplacementUnavailable) {
			t.Fatalf("foreign replacement: %v", err)
		}

		if err := ss.SetNativeStateRoot(ctx, "ses-a", "/root"); err != nil {
			t.Fatal(err)
		}
		if err := ss.ClearNativeResume(ctx, "ses-old", "ses-a", "claude", "n1"); err != nil {
			t.Fatal(err)
		}
		// No HTML escaping in a rewritten plan, as SQLite's json_set writes.
		if err := ss.SetNativeStateRoot(ctx, "ses-b2", "/r"); !errors.Is(err, sessionstore.ErrSessionNotFound) {
			t.Fatalf("missing plan: %v", err)
		}
		if err := ss.SetNativeStateRoot(ctx, "ses-a", "/root<&>"); err != nil {
			t.Fatal(err)
		}
		if plan, _ := ss.LaunchPlanJSON(ctx, "ses-a"); !strings.Contains(plan, `"native_state_root":"/root<&>"`) {
			t.Fatalf("plan escaped: %s", plan)
		}
		if err := ss.SetNativeStateRoot(ctx, "ses-a", "/root"); err != nil {
			t.Fatal(err)
		}
		plan, _ := ss.LaunchPlanJSON(ctx, "ses-a")
		var fields map[string]any
		if err := json.Unmarshal([]byte(plan), &fields); err != nil || fields["native_state_root"] != "/root" || fields["resume_provider_session_id"] != nil || fields["keep"] == nil {
			t.Fatalf("plan %s %v", plan, err)
		}
		if m, _ := ss.GetSessionProviderMapping(ctx, "ses-a", "tether", "claude"); m.NativeSessionID != "" {
			t.Fatalf("mapping not cleared: %+v", m)
		}
	})
}

// TestPolicyConcurrentPlanEdits races the two plan edits on one plan: without the row lock a
// Postgres read-modify-write loses the other's change.
func TestPolicyConcurrentPlanEdits(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		for i := range 5 {
			id := "ses-" + string(rune('a'+i))
			polCreate(t, ss, row(id), `{"resume_provider_session_id":"n1"}`)
			polWarm(ctx, s)
			var mu sync.Mutex
			next := 0
			_, errs := polRace(6, func() (struct{}, error) {
				mu.Lock()
				k := next
				next++
				mu.Unlock()
				if k == 0 {
					return struct{}{}, ss.ClearNativeResume(ctx, id, id, "claude", "n1")
				}
				return struct{}{}, ss.SetNativeStateRoot(ctx, id, "/root")
			})
			for _, err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			plan, _ := ss.LaunchPlanJSON(ctx, id)
			var fields map[string]any
			if err := json.Unmarshal([]byte(plan), &fields); err != nil || fields["native_state_root"] != "/root" || fields["resume_provider_session_id"] != nil {
				t.Fatalf("plan %s %v (a concurrent edit was lost)", plan, err)
			}
		}
	})
}

func TestRecoveryCursorsAndRosters(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		if seq, err := ss.RecoveryChannelCursor(ctx, "agent", "news"); err != nil || seq != 0 {
			t.Fatalf("empty cursor %d %v", seq, err)
		}
		polWarm(ctx, s)
		_, errs := polRace(6, func() (struct{}, error) {
			return struct{}{}, ss.AdvanceRecoveryCursors(ctx, "agent", map[string]int64{"news": 7, "ops": 3})
		})
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		_ = ss.AdvanceRecoveryCursors(ctx, "agent", map[string]int64{"news": 5, "ops": 9})
		for ch, want := range map[string]int64{"news": 7, "ops": 9} {
			if seq, _ := ss.RecoveryChannelCursor(ctx, "agent", ch); seq != want {
				t.Fatalf("%s cursor %d, want %d", ch, seq, want)
			}
		}

		polExec(t, s.C(), `INSERT INTO team_runs (run_id, session_group_id, team_id, team_version, payload, created_at) VALUES ('run-b', 'sg-b', 't', 1, ?, 'c')`, []byte(`{"b":1}`))
		polExec(t, s.C(), `INSERT INTO team_runs (run_id, session_group_id, team_id, team_version, payload, created_at) VALUES ('run-a', 'sg-a', 't', 1, ?, 'c')`, []byte(`{"a":1}`))
		polExec(t, s.C(), `INSERT INTO team_rosters (run_id, version, payload) VALUES ('run-b', 1, ?)`, []byte(`{"rb":1}`))
		polExec(t, s.C(), `INSERT INTO team_rosters (run_id, version, payload) VALUES ('run-a', 1, ?)`, []byte(`{"ra":1}`))
		polExec(t, s.C(), `INSERT INTO team_rosters (run_id, version, payload) VALUES ('run-orphan', 1, ?)`, []byte(`{}`))
		got, err := ss.RecoveryRosterPayloads(ctx)
		if err != nil || len(got) != 2 || got[0].RunID != "run-a" || string(got[0].Roster) != `{"ra":1}` || string(got[1].TeamRun) != `{"b":1}` {
			t.Fatalf("rosters %+v %v", got, err)
		}
	})
}

func TestProvidersMappingsAndUsage(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		if err := ss.UpsertSessionProviderMapping(ctx, "", "o", "p", ""); err == nil {
			t.Fatal("empty session accepted")
		}
		_ = ss.UpsertSessionProviderMapping(ctx, "ses-1", "tether", "codex", "")
		_ = ss.UpsertSessionProviderMapping(ctx, "ses-1", "tether", "claude", "n1")
		_ = ss.UpsertSessionProviderMapping(ctx, "ses-1", "Worker", "claude", "n2")
		created, _ := ss.GetSessionProviderMapping(ctx, "ses-1", "tether", "claude")
		clock.Advance(time.Hour)
		_ = ss.UpsertSessionProviderMapping(ctx, "ses-1", "tether", "claude", "n3")
		m, err := ss.GetSessionProviderMapping(ctx, "ses-1", "tether", "claude")
		if err != nil || m.NativeSessionID != "n3" || m.CreatedAt != created.CreatedAt || m.UpdatedAt == created.UpdatedAt {
			t.Fatalf("upsert %+v %v", m, err)
		}
		if _, err := ss.GetSessionProviderMapping(ctx, "ses-1", "x", "y"); !errors.Is(err, sessionstore.ErrProviderMappingNotFound) {
			t.Fatalf("missing: %v", err)
		}
		list, _ := ss.ListSessionProviderMappings(ctx, "ses-1")
		var keys []string
		for _, m := range list {
			keys = append(keys, m.Provider+"/"+m.Owner+"="+m.NativeSessionID)
		}
		if !reflect.DeepEqual(keys, []string{"claude/Worker=n2", "claude/tether=n3", "codex/tether="}) {
			t.Fatalf("list %v", keys)
		}
		if empty, _ := ss.ListSessionProviderMappings(ctx, "none"); empty == nil || len(empty) != 0 {
			t.Fatalf("empty list %v", empty)
		}
		many, _ := ss.SessionProviderMappings(ctx, []string{"ses-1", "none"})
		if len(many) != 1 || len(many["ses-1"]) != 3 {
			t.Fatalf("many %v", many)
		}

		if u, err := ss.GetSessionUsage(ctx, "ses-1"); err != nil || u.Turns != 0 {
			t.Fatalf("empty usage %+v %v", u, err)
		}
		polWarm(ctx, s)
		_, errs := polRace(6, func() (struct{}, error) {
			return struct{}{}, ss.AddSessionTurnUsage(ctx, "ses-1", sessionstore.TurnUsage{Reported: true, InputTokens: 10, OutputTokens: 2, CostUSD: 0.5, Duration: time.Second})
		})
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		_ = ss.AddSessionTurnUsage(ctx, "ses-1", sessionstore.TurnUsage{Failed: true})
		u, _ := ss.GetSessionUsage(ctx, "ses-1")
		if u.Turns != 7 || u.FailedTurns != 1 || u.UsageTurns != 6 || u.InputTokens != 60 || u.OutputTokens != 12 || u.CostUSD != 3 || u.TurnMS != 6000 || u.UpdatedAt == "" {
			t.Fatalf("usage %+v", u)
		}
	})
}
