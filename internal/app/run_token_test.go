package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/runtoken"
	"github.com/hollis-labs/tether/internal/store"
)

func TestRunLaunchEnvironmentBindingAndNoPersistence(t *testing.T) {
	svc, rt, id, ws := credentialLaunch(t)
	if _, err := svc.LaunchSessionWithContext(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, v := range rt.options.Env {
		k, v, _ := strings.Cut(v, "=")
		env[k] = v
	}
	token := env[runtoken.TokenEnv]
	if token == "" || env[runtoken.IDEnv] == "" {
		t.Fatal("run environment missing")
	}
	p, err := identity.NewStore(svc.Store.DB()).Verify(context.Background(), env["TETHER_TOKEN"])
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.VerifyRunToken(token, p)
	if err != nil || c.RunID != env[runtoken.IDEnv] {
		t.Fatal("launcher token binding failed")
	}
	// A new Service instance reads the same local signing material after restart.
	reopened := &Service{Store: svc.Store}
	if _, err := reopened.VerifyRunToken(token, p); err != nil {
		t.Fatal("restart lost signing key")
	}
	ctx := runtoken.WithClaims(identity.WithPrincipal(context.Background(), p), c)
	snap := api.ResolveCallerContext(ctx, svc.Store, nil)
	if !snap.Verified || !snap.SessionResolved || !snap.RunVerified || snap.ExecutionRunID != c.RunID || snap.TokenJTI != c.JTI {
		t.Fatal("run attribution not additive")
	}
	for _, mutate := range []func(*identity.Principal){func(p *identity.Principal) { p.SessionID = "other" }, func(p *identity.Principal) { p.ID = "other-principal" }, func(p *identity.Principal) { p.Verified = false }, func(p *identity.Principal) { p.Kind = "service" }} {
		bad := p
		mutate(&bad)
		if _, err := svc.VerifyRunToken(token, bad); !errors.Is(err, runtoken.ErrInvalid) {
			t.Fatal("mismatched principal accepted")
		}
	}
	row, err := svc.Store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	next, err := svc.mintRunEnvironment(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	child, err := svc.VerifyRunToken(next[runtoken.TokenEnv], p)
	if err != nil || child.Parent != c.RunID || child.RunID == c.RunID || child.JTI == c.JTI {
		t.Fatal("run uniqueness/parent lost")
	}
	// Persisted plan, rendered config and telemetry contain identifiers only.
	plan, err := svc.Store.GetLaunchPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(plan)
	if strings.Contains(string(raw), token) || plan.Env[runtoken.TokenEnv] != "" {
		t.Fatal("run token persisted in plan")
	}
	if err := filepath.WalkDir(filepath.Join(ws, "boot"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			if strings.Contains(string(b), token) {
				t.Fatal("run token persisted in planted config")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET task_id='reassigned' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	stale := api.ResolveCallerContext(ctx, svc.Store, nil)
	if stale.RunVerified || stale.ExecutionRunID != "" || stale.TokenJTI != "" {
		t.Fatal("reassigned snapshot retained stale signed context")
	}
	if _, err := svc.VerifyRunToken(token, p); !errors.Is(err, runtoken.ErrInvalid) {
		t.Fatal("signed task disagreement accepted")
	}
}

func TestWorkerLaunchReceivesUniqueBoundRunEnvironment(t *testing.T) {
	rig := newIdentityRig(t, "session-cred", "agent-token")
	spec, err := rig.launch("worker-run", "")
	if err != nil {
		t.Fatal(err)
	}
	token := spec.Env[runtoken.TokenEnv]
	p, err := rig.ids.Verify(context.Background(), spec.Credential)
	if err != nil {
		t.Fatal(err)
	}
	c, err := rig.svc.VerifyRunToken(token, p)
	if err != nil || c.RunID != spec.Env[runtoken.IDEnv] || c.Session != spec.Session {
		t.Fatal("worker launch lost run binding")
	}
	plan, err := rig.db.GetLaunchPlan(spec.Session)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(plan)
	if strings.Contains(string(b), token) {
		t.Fatal("worker run token persisted in plan")
	}
}

func TestRunKeyCustodyDoesNotTrustPermissiveDatabaseOrWAL(t *testing.T) {
	svc, _, id, _ := credentialLaunch(t)
	path := svc.Store.RunSigningKeyPath()
	dbPath := strings.TrimSuffix(path, ".run-keys/signing.key")
	if err := os.Chmod(dbPath, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dbPath+"-wal", 0644); err != nil {
		t.Fatal(err)
	}
	row, err := svc.Store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	env, err := svc.mintRunEnvironment(context.Background(), row)
	if err != nil || env[runtoken.TokenEnv] == "" {
		t.Fatal("private custody unavailable beside permissive database")
	}
	key, err := svc.runTokens.Key(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, key) || bytes.Contains(b, key[:32]) {
			t.Fatal("private key persisted in ordinary database/sidecars")
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("key file custody not private")
	}
	info, err = os.Stat(filepath.Dir(path))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("key directory custody not private")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.mintRunEnvironment(context.Background(), row); !errors.Is(err, runtoken.ErrCustody) {
		t.Fatal("cached authority bypassed unsafe key custody")
	}
}

func TestExecutionRunIDsPreserveKnownAndAbsentTorqueOnLocalAndWorker(t *testing.T) {
	for _, torque := range []string{"", "42"} {
		t.Run("local-"+torque, func(t *testing.T) {
			for _, key := range []string{"TETHER_RUN_ID", runtoken.IDEnv, runtoken.TokenEnv} {
				t.Setenv(key, "ambient-wrong")
			}
			svc, rt, id, _ := credentialLaunch(t)
			var value any
			if torque != "" {
				value = torque
			}
			if _, err := svc.Store.DB().Exec(`UPDATE sessions SET run_id=? WHERE id=?`, value, id); err != nil {
				t.Fatal(err)
			}
			plan, err := svc.Store.GetLaunchPlan(id)
			if err != nil {
				t.Fatal(err)
			}
			plan.Env["TETHER_RUN_ID"] = "provider-wrong"
			plan.Env[runtoken.IDEnv] = "provider-wrong"
			plan.Env[runtoken.TokenEnv] = "provider-wrong"
			raw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=? WHERE session_id=?`, string(raw), id); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.LaunchSession(id); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			counts := map[string]int{}
			for _, entry := range rt.options.Env {
				k, v, _ := strings.Cut(entry, "=")
				env[k] = v
				counts[k]++
			}
			if env["TETHER_RUN_ID"] != torque || counts[runtoken.IDEnv] != 1 || (torque == "" && counts["TETHER_RUN_ID"] != 0) || (torque != "" && counts["TETHER_RUN_ID"] != 1) {
				t.Fatal("local reserved env override or duplicate")
			}
			p, err := identity.NewStore(svc.Store.DB()).Verify(context.Background(), env["TETHER_TOKEN"])
			if err != nil {
				t.Fatal(err)
			}
			c, err := svc.VerifyRunToken(env[runtoken.TokenEnv], p)
			if err != nil || c.RunID != env[runtoken.IDEnv] || c.RunID == torque {
				t.Fatal("execution token reused Torque identity")
			}
			assertRunIDMapping(t, svc, id, p, c, torque)
		})
		t.Run("worker-"+torque, func(t *testing.T) {
			for _, key := range []string{"TETHER_RUN_ID", runtoken.IDEnv, runtoken.TokenEnv} {
				t.Setenv(key, "ambient-wrong")
			}
			rig := newIdentityRig(t, "session-cred", "agent-token")
			id := "worker-execution"
			row := store.SessionRow{ID: id, WorkerID: rig.worker, LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderKind: "cli", Workspace: t.TempDir(), State: "created", RunID: sql.NullString{String: torque, Valid: torque != ""}}
			plan := &launch.Plan{LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderBrand: "claude", Env: map[string]string{"TETHER_RUN_ID": "provider-wrong", runtoken.IDEnv: "provider-wrong", runtoken.TokenEnv: "provider-wrong"}}
			if err := rig.db.CreateSession(row, plan); err != nil {
				t.Fatal(err)
			}
			if _, err := rig.svc.launchWorkerSession(context.Background(), &row, plan); err != nil {
				t.Fatal(err)
			}
			spec := <-rig.specs
			_, present := spec.Env["TETHER_RUN_ID"]
			if spec.Env["TETHER_RUN_ID"] != torque || (torque == "" && present) {
				t.Fatal("worker replaced Torque identity")
			}
			p, err := rig.ids.Verify(context.Background(), spec.Credential)
			if err != nil {
				t.Fatal(err)
			}
			c, err := rig.svc.VerifyRunToken(spec.Env[runtoken.TokenEnv], p)
			if err != nil || c.RunID != spec.Env[runtoken.IDEnv] || c.RunID == torque {
				t.Fatal("worker execution token reused Torque identity")
			}
			assertRunIDMapping(t, rig.svc, id, p, c, torque)
		})
	}
}
func assertRunIDMapping(t *testing.T, svc *Service, id string, p identity.Principal, c runtoken.Claims, torque string) {
	t.Helper()
	row, err := svc.Store.GetSession(id)
	if err != nil || row.RunID.String != torque || row.RunID.Valid != (torque != "") {
		t.Fatal("session Torque authority changed")
	}
	event, ok, err := (stampSessions{db: svc.Store}).Session(context.Background(), id)
	if err != nil || !ok || event.RunID != torque {
		t.Fatal("event-to-Torque run mapping changed")
	}
	legacy := api.ResolveCallerContext(identity.WithPrincipal(context.Background(), p), svc.Store, nil)
	if legacy.RunID != torque || legacy.RunVerified || legacy.ExecutionRunID != "" || legacy.TokenJTI != "" {
		t.Fatal("legacy run attribution upgraded execution")
	}
	signed := api.ResolveCallerContext(runtoken.WithClaims(identity.WithPrincipal(context.Background(), p), c), svc.Store, nil)
	if signed.RunID != torque || !signed.RunVerified || signed.ExecutionRunID != c.RunID || signed.TokenJTI != c.JTI {
		t.Fatal("signed/Torque attribution conflated")
	}
}
