package sessionstore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
	"github.com/hollis-labs/tether/next/internal/store/sessionstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

const (
	repActor   = "msg://agent/team/a1"
	repSrcPlan = `{"team_member":true,"resume_source_session_id":""}`
	repDstPlan = `{"team_member":true,"resume_source_session_id":"ses-src"}`
	repPolicy  = `{"session_id":"ses-src","agent_id":"agent","sealed":true}`
	// repRoster is minified with the members in a fixed order, so the moved payload can be compared
	// byte for byte with what SQLite's json_set writes.
	repRoster = `{"version":1,"members":[{"actor":"msg://agent/team/other","session_id":"ses-other","status":"active"},{"actor":"` + repActor + `","session_id":"ses-src","status":"active"}]}`
	repMember = `{"id":"m1","actor":"` + repActor + `","session_id":"ses-src","status":"active","enrolled":true}`
	// The roster's other member, for the two-member recovery.
	repOther   = "msg://agent/team/other"
	repPolicy2 = `{"session_id":"ses-other","agent_id":"agent","sealed":true}`
	repMember2 = `{"id":"m2","actor":"` + repOther + `","session_id":"ses-other","status":"active","enrolled":true}`
)

// repTeamSource seeds a failed (exit -1) team-member session ses-src with pid 42 and every row of a
// retained team recovery authority: enrollment and session port intents, the host intent and its
// member, the current team binding, the active agent profile pinned to the team definition, the live
// run and its roster, the team idempotency key, a session credential and the sealed MCP policy.
// repDeliverySchema creates go-messaging's tables on SQLite, as the delivery store's activation does
// at open (the replacement's lease check requires them there, as the old store did). Postgres has no
// library store yet (CW-20261115-0013).
func repDeliverySchema(t *testing.T, s *store.Stores) {
	t.Helper()
	db, err := s.LibraryDB(context.Background())
	if errors.Is(err, store.ErrLibraryUnavailable) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := delivery.ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}

func repTeamSource(t *testing.T, s *store.Stores, ss *sessionstore.Store) sessionstore.SessionRow {
	t.Helper()
	ctx := context.Background()
	repDeliverySchema(t, s)
	src := row("ses-src")
	key := &sessionstore.SessionIdempotency{PrincipalID: sessionstore.IdempotencyPrincipalTeam, Key: "team-port:n1", Operation: sessionstore.IdempotencyOpCreate, RequestDigest: "d"}
	if _, err := ss.CreateSession(ctx, src, repSrcPlan, key); err != nil {
		t.Fatal(err)
	}
	if err := ss.UpdateSessionState(ctx, "ses-src", "running", 42, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.SaveSessionMCPPolicy(ctx, "ses-src", "agent", repPolicy); err != nil {
		t.Fatal(err)
	}
	repExec(t, s.A(), `INSERT INTO principals (principal_id, kind, display, token_hash, scopes_json, session_id, addresses_json, created_at)
 VALUES ('msg://session/local/ses-src', 'session', 'd', ?, '["messages:read"]', 'ses-src', '[]', '2026-11-14T00:00:00Z')`, repHash("src"))
	exit := -1
	if err := ss.UpdateSessionState(ctx, "ses-src", "failed", 42, &exit); err != nil {
		t.Fatal(err)
	}
	c := s.C()
	repExec(t, c, `INSERT INTO session_provider_mappings (session_id, owner, provider, native_session_id, created_at, updated_at) VALUES ('ses-src', 'claude', 'claude', 'native-1', 'x', 'x')`)
	repExec(t, c, `INSERT INTO registry_entries (urn, kind, display_name, status, props_json, created_at, updated_at)
 VALUES (?, 'agent', 'a1', 'active', '{"team_definition":{"id":"team","revision":1,"digest":"dg"}}', 'x', 'x')`, repActor)
	repExec(t, c, `INSERT INTO team_runs (run_id, session_group_id, team_id, team_version, payload, created_at) VALUES ('run-1', 'sg-1', 'team', 1, ?, 'x')`, []byte(`{"status":"running"}`))
	repExec(t, c, `INSERT INTO team_rosters (run_id, version, payload) VALUES ('run-1', 1, ?)`, []byte(repRoster))
	repExec(t, c, `INSERT INTO team_host_intents (intent_key, request, member) VALUES ('ik-1', ?, ?)`, []byte(`{"RunID":"run-1"}`), []byte(repMember))
	repExec(t, c, `INSERT INTO team_port_intents (port_kind, intent_key, nonce, payload, state) VALUES ('session', 'ik-1', 'n1', ?, 'done')`, []byte(`"ses-src"`))
	repExec(t, c, `INSERT INTO team_port_intents (port_kind, intent_key, binding_secret, acquired_urn, payload, state) VALUES ('enrollment', 'ik-1', 'att-1', ?, ?, 'done')`,
		repActor, []byte(`{"Enrollment":{"actor":"`+repActor+`","agent_id":"`+repActor+`"},"Pin":{"id":"team","revision":1,"digest":"dg"}}`))
	repExec(t, c, `INSERT INTO runtime_bindings (id, target_urn, session_id, host_id, attempt_id, generation, visibility, leased_at, created_at, updated_at)
 VALUES ('bind-1', ?, 'ses-src', 'team', 'att-1', 1, 'tether-hosted', 'x', 'x', 'x')`, repActor)
	got, err := ss.GetSession(ctx, "ses-src")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// repTeamOther seeds the roster's other member (ses-other, actor repOther) with its own recovery
// authority (ik-2, binding bind-2), sharing run-1 and its roster with repTeamSource.
func repTeamOther(t *testing.T, s *store.Stores, ss *sessionstore.Store) sessionstore.SessionRow {
	t.Helper()
	ctx := context.Background()
	other := row("ses-other")
	key := &sessionstore.SessionIdempotency{PrincipalID: sessionstore.IdempotencyPrincipalTeam, Key: "team-port:n2", Operation: sessionstore.IdempotencyOpCreate, RequestDigest: "d"}
	if _, err := ss.CreateSession(ctx, other, repSrcPlan, key); err != nil {
		t.Fatal(err)
	}
	if err := ss.UpdateSessionState(ctx, "ses-other", "running", 42, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.SaveSessionMCPPolicy(ctx, "ses-other", "agent", repPolicy2); err != nil {
		t.Fatal(err)
	}
	repExec(t, s.A(), `INSERT INTO principals (principal_id, kind, display, token_hash, scopes_json, session_id, addresses_json, created_at)
 VALUES ('msg://session/local/ses-other', 'session', 'd', ?, '["messages:read"]', 'ses-other', '[]', '2026-11-14T00:00:00Z')`, repHash("oth"))
	exit := -1
	if err := ss.UpdateSessionState(ctx, "ses-other", "failed", 42, &exit); err != nil {
		t.Fatal(err)
	}
	c := s.C()
	repExec(t, c, `INSERT INTO session_provider_mappings (session_id, owner, provider, native_session_id, created_at, updated_at) VALUES ('ses-other', 'claude', 'claude', 'native-2', 'x', 'x')`)
	repExec(t, c, `INSERT INTO registry_entries (urn, kind, display_name, status, props_json, created_at, updated_at)
 VALUES (?, 'agent', 'a2', 'active', '{"team_definition":{"id":"team","revision":1,"digest":"dg"}}', 'x', 'x')`, repOther)
	repExec(t, c, `INSERT INTO team_host_intents (intent_key, request, member) VALUES ('ik-2', ?, ?)`, []byte(`{"RunID":"run-1"}`), []byte(repMember2))
	repExec(t, c, `INSERT INTO team_port_intents (port_kind, intent_key, nonce, payload, state) VALUES ('session', 'ik-2', 'n2', ?, 'done')`, []byte(`"ses-other"`))
	repExec(t, c, `INSERT INTO team_port_intents (port_kind, intent_key, binding_secret, acquired_urn, payload, state) VALUES ('enrollment', 'ik-2', 'att-2', ?, ?, 'done')`,
		repOther, []byte(`{"Enrollment":{"actor":"`+repOther+`","agent_id":"`+repOther+`"},"Pin":{"id":"team","revision":1,"digest":"dg"}}`))
	repExec(t, c, `INSERT INTO runtime_bindings (id, target_urn, session_id, host_id, attempt_id, generation, visibility, leased_at, created_at, updated_at)
 VALUES ('bind-2', ?, 'ses-other', 'team', 'att-2', 1, 'tether-hosted', 'x', 'x', 'x')`, repOther)
	got, err := ss.GetSession(ctx, "ses-other")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func repInputOther(source sessionstore.SessionRow, dest string) sessionstore.TeamReplacementInput {
	in := repInput(source, dest)
	in.IntentKey, in.RecoveryActorURI = "ik-2", repOther
	in.PlanJSON = strings.Replace(repDstPlan, "ses-src", "ses-other", 1)
	in.ResealPolicy = func(policy, sourceID, agentID string) (string, error) {
		if policy != repPolicy2 || sourceID != "ses-other" || agentID != "agent" {
			return "", errors.New("policy mismatch")
		}
		return strings.Replace(policy, "ses-other", dest, 1), nil
	}
	return in
}

func repInput(source sessionstore.SessionRow, dest string) sessionstore.TeamReplacementInput {
	return sessionstore.TeamReplacementInput{
		Source: source, SourcePlanJSON: repSrcPlan, DestinationID: dest, Workspace: "/w2", IntentKey: "ik-1",
		PlanJSON: repDstPlan, RecoveryActorURI: repActor, CheckedPID: 42, CredentialMode: "observe",
		CheckPlan: func(plan string) error {
			if plan != repSrcPlan {
				return errors.New("plan changed")
			}
			return nil
		},
		ResealPolicy: func(policy, sourceID, agentID string) (string, error) {
			if policy != repPolicy || sourceID != "ses-src" || agentID != "agent" {
				return "", errors.New("policy mismatch")
			}
			return strings.Replace(policy, "ses-src", dest, 1), nil
		},
	}
}

func repPrepare(ctx context.Context, s *store.Stores, ss *sessionstore.Store, es *eventstore.Store, in sessionstore.TeamReplacementInput) (sessionstore.SessionReplacement, error) {
	var out sessionstore.SessionReplacement
	err := s.Joint(ctx, "PrepareTeamReplacementTx", func(ctx context.Context, tx store.Tx) error {
		var err error
		out, err = ss.PrepareTeamReplacementTx(ctx, tx, es, in)
		return err
	})
	return out, err
}

func repText(t *testing.T, q gostore.Querier, query string, args ...any) string {
	t.Helper()
	var v []byte
	if err := q.QueryRowContext(context.Background(), q.Dialect().Rebind(query), args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return string(v)
}

func TestReplacement(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		es := eventstore.New(s)
		es.Now = clock.Now
		src := repTeamSource(t, s, ss)
		r := s.C().Reader()

		if ok, err := ss.RetainedTeamRecovery(ctx, r, "ses-src"); err != nil || !ok {
			t.Fatalf("retained authority %v %v", ok, err)
		}
		if f, err := ss.TeamRecoveryFenceFor(ctx, r, "ses-src"); err != nil || f.BindingID != "bind-1" || f.ActorURI != repActor || f.IntentKey != "ik-1" || f.BindingGeneration != 1 {
			t.Fatalf("fence %+v %v", f, err)
		}
		if _, err := ss.TeamRecoveryFenceFor(ctx, r, "ses-other"); !errors.Is(err, sessionstore.ErrTeamRecoveryUnavailable) {
			t.Fatalf("fence for a non-member: %v", err)
		}
		if _, found, err := ss.ReplacementCredentialFloor(ctx, "ses-dst"); found || err != nil {
			t.Fatalf("floor before replacement: %v %v", found, err)
		}

		// Refusals: a changed plan, a pid not observed, a session holding shim custody.
		bad := repInput(src, "ses-dst")
		bad.CheckedPID = 41
		if _, err := repPrepare(ctx, s, ss, es, bad); !errors.Is(err, sessionstore.ErrSessionReplacementUnavailable) {
			t.Fatalf("wrong pid: %v", err)
		}
		bad = repInput(src, "ses-dst")
		bad.SourcePlanJSON = `{}`
		if _, err := repPrepare(ctx, s, ss, es, bad); !errors.Is(err, sessionstore.ErrSessionReplacementUnavailable) {
			t.Fatalf("changed plan: %v", err)
		}

		before, _ := es.Pending(ctx, 0, 0)
		got, err := repPrepare(ctx, s, ss, es, repInput(src, "ses-dst"))
		if err != nil || got.ReplacementID != "ses-dst" || got.BindingID != "bind-1" || got.ActorURI != repActor {
			t.Fatalf("prepare %+v %v", got, err)
		}
		dst, err := ss.GetSession(ctx, "ses-dst")
		if err != nil || dst.State != "created" || dst.ParentSessionID != "ses-src" || dst.Intent != "resume" || dst.Workspace != "/w2" || dst.ProviderID != "claude" {
			t.Fatalf("destination %+v %v", dst, err)
		}
		if plan, _ := ss.LaunchPlanJSON(ctx, "ses-dst"); plan != repDstPlan {
			t.Fatalf("destination plan %q", plan)
		}
		if p, _, _ := ss.SessionMCPPolicyJSON(ctx, "ses-dst"); !strings.Contains(p, `"ses-dst"`) {
			t.Fatalf("destination policy %q", p)
		}
		if got := repText(t, r, `SELECT session_id FROM runtime_bindings WHERE id = 'bind-1'`); got != "ses-dst" {
			t.Fatalf("binding on %q", got)
		}
		var member map[string]any
		_ = json.Unmarshal([]byte(repText(t, r, `SELECT member FROM team_host_intents WHERE intent_key = 'ik-1'`)), &member)
		if member["session_id"] != "ses-dst" || member["id"] != "m1" {
			t.Fatalf("host intent member %v", member)
		}
		if got := repText(t, r, `SELECT payload FROM team_port_intents WHERE port_kind = 'session'`); got != `"ses-dst"` {
			t.Fatalf("port payload %q", got)
		}
		if rec, _ := ss.GetSessionIdempotency(ctx, sessionstore.IdempotencyPrincipalTeam, "team-port:n1"); rec == nil || rec.SessionID != "ses-dst" {
			t.Fatalf("team key %+v", rec)
		}
		wantRoster := strings.Replace(strings.Replace(repRoster, `"version":1`, `"version":2`, 1), `"session_id":"ses-src"`, `"session_id":"ses-dst"`, 1)
		if got := repText(t, r, `SELECT payload FROM team_rosters WHERE run_id = 'run-1' AND version = 2`); got != wantRoster {
			t.Fatalf("roster\n got %s\nwant %s", got, wantRoster)
		}
		if got := repText(t, r, `SELECT payload FROM team_roster_snapshots WHERE run_id = 'run-1' AND version = 2`); got != wantRoster {
			t.Fatalf("roster snapshot %s", got)
		}
		if got := repText(t, r, `SELECT native_session_id FROM session_provider_mappings WHERE session_id = 'ses-dst'`); got != "native-1" {
			t.Fatalf("native mapping %q", got)
		}
		var revoked sql.NullString
		if err := s.A().Reader().QueryRowContext(ctx, `SELECT revoked_at FROM principals WHERE session_id = 'ses-src'`).Scan(&revoked); err != nil || !revoked.Valid {
			t.Fatalf("source credential not revoked: %v", err)
		}
		if rep, err := ss.SessionReplacement(ctx, "ses-src"); err != nil || rep != got {
			t.Fatalf("replacement row %+v %v", rep, err)
		}
		if rep, err := ss.SessionReplacementDestination(ctx, "ses-dst"); err != nil || rep != got {
			t.Fatalf("by destination %+v %v", rep, err)
		}
		after, _ := es.Pending(ctx, 0, 0)
		if len(after) != len(before)+1 || after[len(after)-1].Kind != "session.replaced_by" ||
			after[len(after)-1].PayloadJSON != `{"replaced_by":"ses-dst","replaces":"ses-src","actor":"`+repActor+`"}` {
			t.Fatalf("lineage event %+v", after)
		}
		floor, found, err := ss.ReplacementCredentialFloor(ctx, "ses-dst")
		if !found || err != nil || len(floor.Scopes) != 1 || floor.Scopes[0] != "messages:read" || floor.Mode != "observe" {
			t.Fatalf("floor %+v %v %v", floor, found, err)
		}

		// A repeat returns the recorded replacement; another destination is refused.
		if again, err := repPrepare(ctx, s, ss, es, repInput(src, "ses-dst")); err != nil || again != got {
			t.Fatalf("repeat %+v %v", again, err)
		}
		if _, err := repPrepare(ctx, s, ss, es, repInput(src, "ses-elsewhere")); !errors.Is(err, sessionstore.ErrSessionReplacementUnavailable) {
			t.Fatalf("other destination: %v", err)
		}
	})
}

// TestReplacementConcurrent races prepares of one source to one destination: every caller gets the
// one recorded replacement. On Postgres they overlap; without the source row lock the losers peek no
// replacement, then fail on the destination's primary key or a moved binding.
func TestReplacementConcurrent(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		es := eventstore.New(s)
		es.Now = clock.Now
		src := repTeamSource(t, s, ss)
		repWarm(s, 6)
		results, errs := repRace(6, func() (sessionstore.SessionReplacement, error) {
			return repPrepare(ctx, s, ss, es, repInput(src, "ses-dst"))
		})
		for i, err := range errs {
			if err != nil || results[i].ReplacementID != "ses-dst" {
				t.Fatalf("racer %d: %+v %v", i, results[i], err)
			}
		}
		if pending, _ := es.Pending(ctx, 0, 0); len(pending) != 1 {
			t.Fatalf("lineage events %d, want 1", len(pending))
		}
	})
}

// TestReplacedShimDelete: migration 00006's guard keeps a replaced session's shim custody row, on both
// dialects, and leaves other shim rows deletable.
func TestReplacedShimDelete(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		c := s.C()
		for _, id := range []string{"ses-x", "ses-y"} {
			repExec(t, c, `INSERT INTO session_shims (session_id, shim_key, host_backend, socket_path, descriptor_path, runtime, runtime_generation, boot_generation, created_at, updated_at)
 VALUES (?, ?, 'detached', '/s', '/d', 'claude', '1', '1', 'x', 'x')`, id, "key-"+id)
		}
		repExec(t, c, `INSERT INTO session_replacements (source_session_id, replacement_session_id, actor_uri, intent_key, binding_id, binding_generation, source_plan_digest,
 credential_scopes_json, credential_mode, committed_at) VALUES ('ses-x', 'ses-x2', 'a', 'i', 'b', 1, 'd', '[]', 'off', 'x')`)
		err := c.WriteTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM session_shims WHERE session_id = 'ses-x'`)
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "replaced custody history is retained") {
			t.Fatalf("replaced shim delete: %v", err)
		}
		repExec(t, c, `DELETE FROM session_shims WHERE session_id = 'ses-y'`)
	})
}

// TestReplacementDeliveryFences installs the go-messaging guards (SQLite, where the library's tables
// exist; on Postgres it is a no-op) and checks that a replaced execution cannot claim a delivery.
func TestReplacementDeliveryFences(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		if s.C().Dialect().Name() == gostore.DriverPostgres {
			if err := ss.InstallReplacementDeliveryFences(ctx); err != nil {
				t.Fatalf("postgres: %v", err)
			}
			return
		}
		db := storetest.LibraryDB(t, s)
		if err := delivery.ApplySQLiteSchema(ctx, db); err != nil {
			t.Fatal(err)
		}
		for range 2 { // idempotent
			if err := ss.InstallReplacementDeliveryFences(ctx); err != nil {
				t.Fatal(err)
			}
		}
		repExec(t, s.C(), `INSERT INTO session_replacements (source_session_id, replacement_session_id, actor_uri, intent_key, binding_id, binding_generation, source_plan_digest,
 credential_scopes_json, credential_mode, committed_at) VALUES ('ses-old', 'ses-new', 'a', 'i', 'b', 1, 'd', '[]', 'off', 'x')`)
		claim := func(holder string) error {
			_, err := db.ExecContext(ctx, `INSERT INTO messaging_attempts (id, message_id, delivery_id, lease_token, holder, acquired_at, expires_at, stage) VALUES (?, 'm', 'd', 't', ?, 'x', 'x', 'leased')`,
				fmt.Sprint("att-", holder), holder)
			return err
		}
		if err := claim("msg://session/local/ses-old"); err == nil || !strings.Contains(err.Error(), "replaced execution cannot claim delivery") {
			t.Fatalf("replaced claim: %v", err)
		}
		if err := claim("msg://session/local/ses-new"); err != nil {
			t.Fatalf("replacement claim: %v", err)
		}
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'replaced_delivery_%'`).Scan(&n); err != nil || n != 2 {
			t.Fatalf("fence triggers %d %v", n, err)
		}
	})
}

// TestReplacementRefusesWorkerHosted: a worker-hosted source is never replaced (the destination
// would be hub-local while the remote process may be live). The old store refused every such source.
func TestReplacementRefusesWorkerHosted(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		es := eventstore.New(s)
		es.Now = clock.Now
		repTeamSource(t, s, ss)
		repExec(t, s.C(), `UPDATE sessions SET worker_id = 'w1' WHERE id = 'ses-src'`)
		src, err := ss.GetSession(ctx, "ses-src")
		if err != nil || src.WorkerID != "w1" {
			t.Fatalf("source %+v %v", src, err)
		}
		if _, err := repPrepare(ctx, s, ss, es, repInput(src, "ses-dst")); !errors.Is(err, sessionstore.ErrSessionReplacementUnavailable) {
			t.Fatalf("worker-hosted source: %v", err)
		}
		if _, err := ss.GetSession(ctx, "ses-dst"); !errors.Is(err, sessionstore.ErrSessionNotFound) {
			t.Fatalf("a refused replacement created its destination: %v", err)
		}
	})
}

// TestReplacementTwoMembersConcurrent: two members of one team recovered at once (after a host loss)
// both succeed, as on SQLite's one writer: on Postgres the second waits on the roster row and reads
// the first's committed version instead of refusing.
func TestReplacementTwoMembersConcurrent(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		es := eventstore.New(s)
		es.Now = clock.Now
		src := repTeamSource(t, s, ss)
		other := repTeamOther(t, s, ss)
		inputs := []sessionstore.TeamReplacementInput{repInput(src, "ses-dst"), repInputOther(other, "ses-dst2")}
		var next atomic.Int32
		repWarm(s, 4)
		results, errs := repRace(2, func() (sessionstore.SessionReplacement, error) {
			return repPrepare(ctx, s, ss, es, inputs[next.Add(1)-1])
		})
		for i, err := range errs {
			if err != nil {
				t.Fatalf("member %d (%+v): %v", i, results[i], err)
			}
		}
		roster := repText(t, s.C().Reader(), `SELECT payload FROM team_rosters WHERE run_id = 'run-1'`)
		if !strings.Contains(roster, `"version":3`) || !strings.Contains(roster, `"session_id":"ses-dst"`) || !strings.Contains(roster, `"session_id":"ses-dst2"`) {
			t.Fatalf("roster after both recoveries: %s", roster)
		}
	})
}

// TestReplacementRequiresDeliveryTablesOnSQLite: on SQLite a missing messaging_deliveries refuses the
// replacement (the old store's lease check errored there); only Postgres, where the library has no
// store yet, skips the check.
func TestReplacementRequiresDeliveryTablesOnSQLite(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		if s.C().Dialect().Name() == gostore.DriverPostgres {
			t.Skip("NOTICE: Postgres has no go-messaging store yet (CW-20261115-0013); the lease check is skipped there")
		}
		ctx := context.Background()
		ss, clock := newStore(s)
		es := eventstore.New(s)
		es.Now = clock.Now
		src := repTeamSource(t, s, ss)
		db, err := s.LibraryDB(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `DROP TABLE messaging_deliveries`); err != nil {
			t.Fatal(err)
		}
		if _, err := repPrepare(ctx, s, ss, es, repInput(src, "ses-dst")); err == nil {
			t.Fatal("a replacement went through without the delivery table")
		}
		if _, err := ss.GetSession(ctx, "ses-dst"); !errors.Is(err, sessionstore.ErrSessionNotFound) {
			t.Fatalf("a refused replacement created its destination: %v", err)
		}
	})
}
