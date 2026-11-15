package astore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/execpolicy"
	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/astore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

// each runs fn on both dialects with an astore over a fresh migrated database and a fixed clock. raw is
// the underlying store, for fixtures the class handles refuse (DDL).
func each(t *testing.T, fn func(t *testing.T, a *astore.Store, s *store.Stores, raw *gostore.Store, clock *storetest.Clock)) {
	t.Helper()
	for _, d := range []gostore.Driver{gostore.DriverSQLite, gostore.DriverPostgres} {
		t.Run(string(d), func(t *testing.T) {
			raw := storetest.Raw(t, d)
			s := store.New(raw)
			clock := storetest.NewClock()
			fn(t, astore.New(s, clock.Now), s, raw, clock)
		})
	}
}

func exec(t *testing.T, h *store.Handle, q string, args ...any) {
	t.Helper()
	if err := h.WriteTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	}); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestExecutionPolicyRoundTripAndUsage(t *testing.T) {
	each(t, func(t *testing.T, a *astore.Store, s *store.Stores, _ *gostore.Store, _ *storetest.Clock) {
		ctx := context.Background()
		p, err := a.GetExecutionPolicy(ctx)
		if err != nil || p.Paused || p.MaxSessions != 0 {
			t.Fatalf("default = %+v, %v", p, err)
		}
		want := execpolicy.Policy{Paused: true, Reason: "r", MaxSessions: 3, ProjectCaps: map[string]int{"p": 1}}
		if err := a.SetExecutionPolicy(ctx, want); err != nil {
			t.Fatal(err)
		}
		want.MaxSessions = 4 // a second set replaces the row
		if err := a.SetExecutionPolicy(ctx, want); err != nil {
			t.Fatal(err)
		}
		got, err := a.GetExecutionPolicy(ctx)
		if err != nil || !got.Paused || got.MaxSessions != 4 || got.ProjectCaps["p"] != 1 || got.Reason != "r" {
			t.Fatalf("got %+v, %v", got, err)
		}
		exec(t, s.A(), `INSERT INTO logical_agents (id, created_at, updated_at) VALUES ('a', 't', 't')`)
		for i, st := range []string{"created", "running", "orphaned", "completed", "detached", "launching"} {
			project := "p"
			if st == "launching" {
				project = "q"
			}
			exec(t, s.C(), `INSERT INTO sessions (id, launch_id, project_id, logical_agent_id, provider_id, workspace, state, created_at, updated_at)
				VALUES (?, 'l', ?, 'a', 'pv', '/w', ?, 't', 't')`, fmt.Sprintf("s%d", i), project, st)
		}
		u, err := a.ExecutionUsage(ctx)
		if err != nil || u.Total != 4 || u.ByProject["p"] != 3 || u.ByProject["q"] != 1 {
			t.Fatalf("usage = %+v, %v", u, err)
		}
	})
}

const frozenTestEnvironment = "f64b485f-7f8d-441b-89d3-378b70e81352"

func insertLegacyEvents(t *testing.T, s *store.Stores, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		exec(t, s.B(), `INSERT INTO events (scope, at, kind) VALUES ('daemon', '2026-01-01T00:00:00Z', 'legacy')`)
	}
}

func eventIDs(t *testing.T, s *store.Stores) map[int64]string {
	t.Helper()
	rows, err := s.B().Reader().QueryContext(context.Background(), `SELECT id, COALESCE(event_id, '') FROM events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var eid string
		if err := rows.Scan(&id, &eid); err != nil {
			t.Fatal(err)
		}
		out[id] = eid
	}
	return out
}

func TestFrozenEventIDsResumeWithoutChangingExistingIDs(t *testing.T) {
	each(t, func(t *testing.T, a *astore.Store, s *store.Stores, raw *gostore.Store, clock *storetest.Clock) {
		ctx := context.Background()
		insertLegacyEvents(t, s, 5)
		early := "evt_0123456789abcdef0123456789abcdef"
		exec(t, s.B(), `UPDATE events SET event_id = ? WHERE id = 3`, early)
		seeded, err := a.SeedEventIdentity(ctx, frozenTestEnvironment)
		if err != nil || seeded.Cutover != 5 || seeded.Remaining != 4 {
			t.Fatalf("cutover excludes or overwrites legacy rows: %+v %v", seeded, err)
		}
		newID := "evt_new"
		exec(t, s.B(), `INSERT INTO events (scope, at, kind, event_id) VALUES ('daemon', 't', 'new', ?)`, newID)
		part, err := a.BackfillEventIDs(ctx, 2)
		if err != nil || part.NextLegacyID != 3 || part.Remaining != 2 {
			t.Fatalf("chunk boundary: %+v %v", part, err)
		}
		ids := eventIDs(t, s)
		if ids[5] != frozenTestEnvironment+":5" || ids[1] != "" {
			t.Fatalf("backfill did not start at the newest legacy rows: %v", ids)
		}
		// A restart (a new astore over the same database) keeps the cutover and progress.
		again := astore.New(store.New(raw), clock.Now)
		resumed, err := again.SeedEventIdentity(ctx, frozenTestEnvironment)
		if err != nil || resumed != part {
			t.Fatalf("restart reset cutover/progress: %+v %v", resumed, err)
		}
		if _, err := again.SeedEventIdentity(ctx, "38fd4cce-66a0-4d28-b09e-2387fb6b577d"); !errors.Is(err, astore.ErrEventIdentityMismatch) {
			t.Fatalf("identity mismatch accepted: %v", err)
		}
		for i := 0; i < 3; i++ {
			if resumed, err = again.BackfillEventIDs(ctx, 1); err != nil {
				t.Fatal(err)
			}
		}
		if resumed.Remaining != 0 || resumed.Backfilled != 4 || resumed.Cutover != 5 || resumed.Dropped != 0 {
			t.Fatalf("completion: %+v", resumed)
		}
		for id, eid := range eventIDs(t, s) {
			want := fmt.Sprintf("%s:%d", frozenTestEnvironment, id)
			switch id {
			case 3:
				want = early
			case 6:
				want = newID
			}
			if eid != want {
				t.Fatalf("row %d id = %q, want %q", id, eid, want)
			}
		}
		done, err := again.BackfillEventIDs(ctx, 1)
		if err != nil || done != resumed {
			t.Fatalf("a completed run changed: %+v %v", done, err)
		}
	})
}

// failTrigger makes an UPDATE of event_id on row 1 fail, per dialect, to inject a chunk failure.
var failTrigger = map[gostore.Driver][]string{
	gostore.DriverSQLite: {`CREATE TRIGGER fail_event_id BEFORE UPDATE OF event_id ON events WHEN NEW.id = 1 BEGIN SELECT RAISE(ABORT, 'injected failure'); END`},
	gostore.DriverPostgres: {
		`CREATE FUNCTION fail_event_id_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$`,
		`CREATE TRIGGER fail_event_id BEFORE UPDATE OF event_id ON events FOR EACH ROW WHEN (NEW.id = 1) EXECUTE FUNCTION fail_event_id_fn()`,
	},
}

var dropFailTrigger = map[gostore.Driver]string{
	gostore.DriverSQLite:   `DROP TRIGGER fail_event_id`,
	gostore.DriverPostgres: `DROP TRIGGER fail_event_id ON events`,
}

func TestFrozenEventIDChunkFailureIsAtomicAndResumable(t *testing.T) {
	each(t, func(t *testing.T, a *astore.Store, s *store.Stores, raw *gostore.Store, _ *storetest.Clock) {
		ctx := context.Background()
		insertLegacyEvents(t, s, 2)
		before, err := a.SeedEventIdentity(ctx, frozenTestEnvironment)
		if err != nil {
			t.Fatal(err)
		}
		d := raw.Dialect().Name()
		for _, q := range failTrigger[d] {
			if _, err := raw.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := a.BackfillEventIDs(ctx, 2); err == nil || !strings.Contains(err.Error(), "injected failure") {
			t.Fatalf("injected chunk failure: %v", err)
		}
		after, err := a.EventIdentityProgress(ctx)
		if err != nil || after != before {
			t.Fatalf("a failed chunk advanced progress: %+v %v", after, err)
		}
		for id, eid := range eventIDs(t, s) {
			if eid != "" {
				t.Fatalf("a failed chunk committed a partial update (row %d)", id)
			}
		}
		if err := a.RecordEventIDBackfillFailure(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(ctx, dropFailTrigger[d]); err != nil {
			t.Fatal(err)
		}
		after, err = a.BackfillEventIDs(ctx, 2)
		if err != nil || after.Remaining != 0 || after.Failed != 1 || after.Backfilled != 2 || after.Dropped != 0 {
			t.Fatalf("recovery: %+v %v", after, err)
		}
	})
}

func TestFrozenEventIDRequiresSeedAndValidIdentity(t *testing.T) {
	each(t, func(t *testing.T, a *astore.Store, _ *store.Stores, _ *gostore.Store, _ *storetest.Clock) {
		ctx := context.Background()
		if _, err := a.BackfillEventIDs(ctx, 10); !errors.Is(err, astore.ErrEventIdentityUnseeded) {
			t.Fatalf("unseeded backfill: %v", err)
		}
		if err := a.RecordEventIDBackfillFailure(ctx); !errors.Is(err, astore.ErrEventIdentityUnseeded) {
			t.Fatalf("unseeded failure counter: %v", err)
		}
		for _, id := range []string{"", "../other", "not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
			if _, err := a.SeedEventIdentity(ctx, id); err == nil {
				t.Fatalf("accepted environment %q", id)
			}
		}
		for _, limit := range []int{0, 1001} {
			if _, err := a.BackfillEventIDs(ctx, limit); err == nil {
				t.Fatalf("accepted limit %d", limit)
			}
		}
	})
}

func TestFrozenEventIDStartupRefusesBadRollbackWrites(t *testing.T) {
	each(t, func(t *testing.T, a *astore.Store, s *store.Stores, raw *gostore.Store, _ *storetest.Clock) {
		ctx := context.Background()
		insertLegacyEvents(t, s, 1)
		before, err := a.SeedEventIdentity(ctx, frozenTestEnvironment)
		if err != nil {
			t.Fatal(err)
		}
		// Simulate an unsupported build without the insert guard.
		drop := `DROP TRIGGER events_require_id_after_seed`
		if raw.Dialect().Name() == gostore.DriverPostgres {
			drop += ` ON events`
		}
		if _, err := raw.ExecContext(ctx, drop); err != nil {
			t.Fatal(err)
		}
		insertLegacyEvents(t, s, 1)
		if _, err := a.SeedEventIdentity(ctx, frozenTestEnvironment); !errors.Is(err, astore.ErrEventIDsAfterCutover) {
			t.Fatalf("bad rollback accepted: %v", err)
		}
		after, err := a.EventIdentityProgress(ctx)
		if err != nil || after != before {
			t.Fatalf("bad rollback moved the cutover: %+v %v", after, err)
		}
		// The repair never folds the invalid new row into the legacy namespace.
		if _, err := a.BackfillEventIDs(ctx, 10); err != nil {
			t.Fatal(err)
		}
		if id := eventIDs(t, s)[2]; id != "" {
			t.Fatalf("post-cutover row silently backfilled: %q", id)
		}
	})
}

func TestUpsertLogicalAgentInsertAndUpdate(t *testing.T) {
	each(t, func(t *testing.T, a *astore.Store, s *store.Stores, _ *gostore.Store, clock *storetest.Clock) {
		ctx := context.Background()
		first := clock.Now().Format(time.RFC3339)
		if err := a.UpsertLogicalAgent(ctx, "claude", "backend, storage", "Claude Code"); err != nil {
			t.Fatal(err)
		}
		got, err := a.GetLogicalAgent(ctx, "claude")
		if err != nil || got.Name != "Claude Code" || got.Role != "backend, storage" || got.CreatedAt != first || got.UpdatedAt != first {
			t.Fatalf("after insert: %+v %v", got, err)
		}
		// An operator-set policy column survives a reseed.
		if err := a.SetLogicalAgentPolicyColumns(ctx, "claude", "auto", `{"x":1}`); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Hour)
		second := clock.Now().Format(time.RFC3339)
		if err := a.UpsertLogicalAgent(ctx, "claude", "backend", "Claude Code CLI"); err != nil {
			t.Fatal(err)
		}
		got, err = a.GetLogicalAgent(ctx, "claude")
		if err != nil || got.Name != "Claude Code CLI" || got.Role != "backend" || got.CreatedAt != first || got.UpdatedAt != second ||
			got.CheckpointPolicy != "auto" || got.PoliciesJSON != `{"x":1}` {
			t.Fatalf("after update: %+v %v", got, err)
		}
		if err := a.UpsertLogicalAgent(ctx, "", "r", "n"); err == nil {
			t.Fatal("empty id accepted")
		}
		if _, err := a.GetLogicalAgent(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("missing row: %v, want sql.ErrNoRows", err)
		}
		_ = s
	})
}

func TestListLogicalAgentsOrdersByID(t *testing.T) {
	each(t, func(t *testing.T, a *astore.Store, _ *store.Stores, _ *gostore.Store, _ *storetest.Clock) {
		ctx := context.Background()
		for _, id := range []string{"zeta", "Alpha", "alpha", "mid"} {
			if err := a.UpsertLogicalAgent(ctx, id, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := a.ListLogicalAgents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		// Bytewise order on both dialects (COLLATE "C" on Postgres, BINARY on SQLite).
		if got := strings.Join(ids, ","); got != "Alpha,alpha,mid,zeta" {
			t.Fatalf("order = %s", got)
		}
		if rows[0].Role != "" || rows[0].Name != "" {
			t.Fatalf("NULL columns should read as empty strings: %+v", rows[0])
		}
	})
}

func TestLogicalAgentUpdatesNeedARow(t *testing.T) {
	each(t, func(t *testing.T, a *astore.Store, _ *store.Stores, _ *gostore.Store, _ *storetest.Clock) {
		ctx := context.Background()
		for name, err := range map[string]error{
			"launch id":         a.SetLogicalAgentLaunchID(ctx, "missing", "l"),
			"policy":            a.SetLogicalAgentPolicyColumns(ctx, "missing", "auto", ""),
			"claude session id": a.SetClaudeSessionID(ctx, "missing", "c"),
		} {
			if err == nil || !strings.Contains(err.Error(), "no logical_agents row") {
				t.Errorf("%s on a missing row: %v", name, err)
			}
		}
		if err := a.UpsertLogicalAgent(ctx, "a", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := a.SetLogicalAgentLaunchID(ctx, "a", "launch-1"); err != nil {
			t.Fatal(err)
		}
		if got, _ := a.GetLogicalAgent(ctx, "a"); got.LaunchID != "launch-1" {
			t.Fatalf("launch id = %q", got.LaunchID)
		}
	})
}

func TestClaudeSessionID(t *testing.T) {
	each(t, func(t *testing.T, a *astore.Store, _ *store.Stores, _ *gostore.Store, _ *storetest.Clock) {
		ctx := context.Background()
		if err := a.UpsertLogicalAgent(ctx, "a", "", ""); err != nil {
			t.Fatal(err)
		}
		if got, err := a.GetClaudeSessionID(ctx, "a"); err != nil || got != "" {
			t.Fatalf("default = %q, %v", got, err)
		}
		for _, sid := range []string{"one", "two", ""} { // "" must not clobber "two"
			if err := a.SetClaudeSessionID(ctx, "a", sid); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := a.GetClaudeSessionID(ctx, "a"); err != nil || got != "two" {
			t.Fatalf("got %q, %v; want two", got, err)
		}
		before, _ := a.GetLogicalAgent(ctx, "a")
		if before.UpdatedAt != before.CreatedAt {
			t.Fatal("setting the claude session id touched updated_at")
		}
		if _, err := a.GetClaudeSessionID(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("missing row: %v", err)
		}
		if err := a.SetClaudeSessionID(ctx, "", "x"); err == nil {
			t.Fatal("empty agent id accepted")
		}
	})
}

func TestSettingsCRUD(t *testing.T) {
	each(t, func(t *testing.T, a *astore.Store, _ *store.Stores, _ *gostore.Store, clock *storetest.Clock) {
		ctx := context.Background()
		for _, bad := range []astore.Setting{
			{Scope: "nope", Key: "k", ValueJSON: "1"},
			{Scope: astore.ScopeGlobal, ScopeID: "x", Key: "k", ValueJSON: "1"},
			{Scope: astore.ScopeProject, Key: "k", ValueJSON: "1"},
			{Scope: astore.ScopeUser, ScopeID: "u", ValueJSON: "1"},
		} {
			if err := a.SetSetting(ctx, bad); err == nil {
				t.Errorf("accepted %+v", bad)
			}
		}
		if _, err := a.GetSetting(ctx, astore.ScopeGlobal, "", "k"); !errors.Is(err, astore.ErrSettingNotFound) {
			t.Fatalf("missing: %v", err)
		}
		set := func(s astore.Setting) {
			t.Helper()
			if err := a.SetSetting(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		set(astore.Setting{Scope: astore.ScopeGlobal, Key: "b", ValueJSON: `"g"`})
		set(astore.Setting{Scope: astore.ScopeGlobal, Key: "a", ValueJSON: `1`})
		set(astore.Setting{Scope: astore.ScopeProject, ScopeID: "p", Key: "a", ValueJSON: `2`})
		clock.Advance(90 * time.Second)
		set(astore.Setting{Scope: astore.ScopeGlobal, Key: "a", ValueJSON: `3`}) // update
		got, err := a.GetSetting(ctx, astore.ScopeGlobal, "", "a")
		if err != nil || got.ValueJSON != "3" || !got.UpdatedAt.Equal(clock.Now().Truncate(time.Second)) {
			t.Fatalf("get = %+v, %v (want updated_at %v)", got, err, clock.Now())
		}
		list, err := a.ListSettings(ctx, astore.ScopeGlobal, "")
		if err != nil || len(list) != 2 || list[0].Key != "a" || list[1].Key != "b" {
			t.Fatalf("list = %+v, %v", list, err)
		}
		if err := a.DeleteSetting(ctx, astore.ScopeGlobal, "", "a"); err != nil {
			t.Fatal(err)
		}
		if err := a.DeleteSetting(ctx, astore.ScopeGlobal, "", "a"); err != nil {
			t.Fatalf("deleting a missing setting: %v", err)
		}
		if _, err := a.GetSetting(ctx, astore.ScopeGlobal, "", "a"); !errors.Is(err, astore.ErrSettingNotFound) {
			t.Fatalf("after delete: %v", err)
		}
		if p, err := a.GetSetting(ctx, astore.ScopeProject, "p", "a"); err != nil || p.ValueJSON != "2" {
			t.Fatalf("project setting = %+v, %v", p, err)
		}
	})
}

// TestSettingsReadOldRows: rows the old store wrote (CURRENT_TIMESTAMP text) and rows with RFC3339 text
// read back with their time.
func TestSettingsReadOldRows(t *testing.T) {
	each(t, func(t *testing.T, a *astore.Store, s *store.Stores, _ *gostore.Store, _ *storetest.Clock) {
		ctx := context.Background()
		exec(t, s.A(), `INSERT INTO settings (scope, scope_id, key, value_json, updated_at) VALUES ('global', '', 'old', '1', '2026-01-02 03:04:05')`)
		exec(t, s.A(), `INSERT INTO settings (scope, scope_id, key, value_json, updated_at) VALUES ('global', '', 'rfc', '1', '2026-01-02T03:04:05Z')`)
		want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		for _, k := range []string{"old", "rfc"} {
			got, err := a.GetSetting(ctx, astore.ScopeGlobal, "", k)
			if err != nil || !got.UpdatedAt.Equal(want) {
				t.Errorf("%s: %+v, %v", k, got, err)
			}
		}
	})
}
