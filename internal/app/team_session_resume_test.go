package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

// Seed already-owned durable receipts around a real session runtime. This
// fixture does not provision enrollment or exercise a new authority path.
func retainedRecoveryRig(t *testing.T) (*codexRig, string, string) {
	t.Helper()
	r := newCodexRig(t)
	ctx := context.Background()
	id := r.start()
	if err := r.turn(id, "original team work"); err != nil {
		t.Fatal(err)
	}
	r.wait(1)
	if err := r.svc.StopSession(id); err != nil {
		t.Fatal(err)
	}
	r.ended(id)
	r.svc.Registry = registry.NewService(registry.NewStorage(r.svc.Store.DB()))
	actor, err := r.svc.Registry.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Retained member", Props: map[string]string{"team_definition": `{"id":"worker","revision":"r1"}`}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.svc.Registry.LeaseBinding(ctx, actor.URN, id, "team", "owned-attempt", nil, registry.VisibilityTetherHosted, 0)
	if err != nil {
		t.Fatal(err)
	}
	db := r.svc.Store.DB()
	statements := []struct {
		q string
		a []any
	}{
		{`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.team_member',json('true')) WHERE session_id=?`, []any{id}},
		{`UPDATE sessions SET state='failed',exit_code=-1 WHERE id=?`, []any{id}},
		{`INSERT INTO team_runs(run_id,session_group_id,team_id,team_version,payload,created_at) VALUES('run','group','team',1,'{"status":"running"}','now')`, nil},
		{`INSERT INTO team_host_intents(intent_key,request,member) VALUES('retained',?,?)`, []any{[]byte(`{"RunID":"run"}`), []byte(`{"session_id":"` + id + `","actor":"` + actor.URN + `","status":"active","enrolled":true}`)}},
		{`INSERT INTO team_rosters(run_id,version,payload) VALUES('run',1,?)`, []any{[]byte(`{"members":[{"session_id":"` + id + `","actor":"` + actor.URN + `","status":"active","enrolled":true}]}`)}},
		{`INSERT INTO team_port_intents(port_kind,intent_key,state,request,payload) VALUES('session','retained','done',?,?)`, []any{[]byte(`{"Actor":"` + actor.URN + `"}`), json.RawMessage(`"` + id + `"`)}},
		{`INSERT INTO team_port_intents(port_kind,intent_key,state,acquired_urn,binding_secret,payload) VALUES('enrollment','retained','done',?,'owned-attempt',?)`, []any{actor.URN, []byte(`{"Enrollment":{"actor":"` + actor.URN + `","agent_id":"` + actor.URN + `"},"Pin":{"id":"worker","revision":"r1"}}`)}},
	}
	for _, s := range statements {
		if _, err = db.Exec(s.q, s.a...); err != nil {
			t.Fatal(err)
		}
	}
	return r, id, actor.URN
}

func TestTeamRecoveryResetsSameCanonicalSessionAndRetainsBinding(t *testing.T) {
	r, id, actor := retainedRecoveryRig(t)
	ctx := context.Background()
	if revoked := r.svc.revokeEndedSessionBindings(ctx); revoked != 0 {
		t.Fatalf("startup revoked valid retained ownership: %d", revoked)
	}
	before, err := r.svc.Registry.CurrentBinding(ctx, actor)
	if err != nil {
		t.Fatal(err)
	}
	row, err := r.svc.Store.GetSession(id)
	if err != nil || !r.svc.retainedTeamRecoveryEligible(ctx, row) {
		t.Fatalf("retained eligibility=%v %v", row, err)
	}
	if err = r.svc.RecoverTeamSession(ctx, "retained", id); err != nil {
		t.Fatal(err)
	}
	r.wait(2)
	r.idle(id)
	row, err = r.svc.Store.GetSession(id)
	if err != nil || row.State != "running" {
		t.Fatalf("same-ID recovery=%+v %v", row, err)
	}
	after, err := r.svc.Registry.CurrentBinding(ctx, actor)
	if err != nil || before.ID != after.ID || before.Generation != after.Generation || after.SessionID != id {
		t.Fatalf("identity replacement=%+v %v", after, err)
	}
	plan, err := r.svc.Store.GetLaunchPlan(id)
	if err != nil || plan.RecoveryActorURI != actor || plan.ResumeSourceSessionID != id {
		t.Fatalf("context identity=%+v %v", plan, err)
	}
}

func TestTeamRecoveryRefusesLostOwnershipAndTerminalSessions(t *testing.T) {
	for _, fence := range []string{
		`UPDATE team_host_intents SET tombstone='retire'`,
		`UPDATE team_rosters SET payload='{"members":[]}'`,
		`UPDATE team_runs SET payload='{"status":"completed"}'`,
		`UPDATE team_port_intents SET ended='stop' WHERE port_kind='session'`,
		`UPDATE runtime_bindings SET revoked_at='revoked' WHERE host_id='team'`,
		`UPDATE sessions SET state='killed'`,
		`UPDATE registry_entries SET status='inactive'`,
		`UPDATE registry_entries SET props_json='{}'`,
	} {
		t.Run(fence, func(t *testing.T) {
			r, id, _ := retainedRecoveryRig(t)
			if _, err := r.svc.Store.DB().Exec(fence); err != nil {
				t.Fatal(err)
			}
			if err := r.svc.RecoverTeamSession(context.Background(), "retained", id); err == nil {
				t.Fatal("lost ownership recovered")
			}
			if r.count() != 1 {
				t.Fatal("refusal started a provider")
			}
		})
	}
}
