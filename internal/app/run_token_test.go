package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/runtoken"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
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
	token := testRunProof(t, env)
	if env[runtoken.TokenEnv] != "" || env[runtoken.FileEnv] == "" {
		t.Fatal("provider received raw run proof")
	}
	if info, err := os.Stat(env[runtoken.FileEnv]); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("provider proof file not private")
	}
	if serialized, err := json.Marshal(env); err != nil || strings.Contains(string(serialized), token) {
		t.Fatal("shell snapshot would contain run proof")
	}
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
	rig := newIdentityRig(t, "session-cred", "agent-token", workerwire.FeatureRunTokenFile)
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
	if err != nil || testRunProof(t, env) == "" {
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
			c, err := svc.VerifyRunToken(testRunProof(t, env), p)
			if err != nil || c.RunID != env[runtoken.IDEnv] || c.RunID == torque {
				t.Fatal("execution token reused Torque identity")
			}
			assertRunIDMapping(t, svc, id, p, c, torque)
		})
		t.Run("worker-"+torque, func(t *testing.T) {
			for _, key := range []string{"TETHER_RUN_ID", runtoken.IDEnv, runtoken.TokenEnv} {
				t.Setenv(key, "ambient-wrong")
			}
			rig := newIdentityRig(t, "session-cred", "agent-token", workerwire.FeatureRunTokenFile)
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

func TestUnsupportedExecutionHandoffPreservesLocalLaunch(t *testing.T) {
	svc, rt, id, _ := credentialLaunch(t)
	svc.runTokenSupport = func() bool { return false }
	for _, key := range []string{"TETHER_RUN_ID", runtoken.IDEnv, runtoken.TokenEnv} {
		t.Setenv(key, "ambient-wrong")
	}
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET run_id='42' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	plan, err := svc.Store.GetLaunchPlan(id)
	if err != nil {
		t.Fatal(err)
	}
	plan.Env[runtoken.TokenEnv] = "provider-token"
	plan.Env[runtoken.IDEnv] = "provider-id"
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=? WHERE session_id=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LaunchSession(id); err != nil {
		t.Fatal("optional correlation broke unsupported-platform launch", err)
	}
	env := map[string]string{}
	for _, entry := range rt.options.Env {
		k, v, _ := strings.Cut(entry, "=")
		env[k] = v
	}
	if testRunProof(t, env) != "" || env[runtoken.IDEnv] == "" || env[runtoken.IDEnv] == "provider-id" || env["TETHER_RUN_ID"] != "42" {
		t.Fatal("unsupported local environment lost legacy authority")
	}
	if _, err := os.Stat(svc.Store.RunSigningKeyPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsupported launch created signing material")
	}
	p, err := identity.NewStore(svc.Store.DB()).Verify(context.Background(), env["TETHER_TOKEN"])
	if err != nil {
		t.Fatal("legacy authentication failed", err)
	}
	snapshot := api.ResolveCallerContext(identity.WithPrincipal(context.Background(), p), svc.Store, nil)
	if snapshot.RunVerified || snapshot.ExecutionRunID != "" || snapshot.RunID != "42" {
		t.Fatal("unsupported local execution became verified")
	}
	// Delivery capability never weakens verification of a supplied token.
	row, err := svc.Store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := svc.mintRunEnvironmentFor(context.Background(), row, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyRunToken(signed[runtoken.TokenEnv], p); err != nil {
		t.Fatal("unsupported local delivery disabled remote token verification")
	}
	if _, err := svc.VerifyRunToken("explicit-invalid-token", p); !errors.Is(err, runtoken.ErrInvalid) {
		t.Fatal("invalid supplied token downgraded")
	}
}
func TestWorkerWithoutHandoffFeatureKeepsLegacy(t *testing.T) {
	for _, torque := range []string{"", "42"} {
		t.Run("torque-"+torque, func(t *testing.T) {
			rig := newIdentityRig(t, "session-cred", "agent-token")
			id := "legacy-handoff"
			row := store.SessionRow{ID: id, WorkerID: rig.worker, LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderKind: "cli", Workspace: t.TempDir(), State: "created", RunID: sql.NullString{String: torque, Valid: torque != ""}}
			plan := &launch.Plan{LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderBrand: "claude", Env: map[string]string{runtoken.IDEnv: "provider-id", runtoken.TokenEnv: "provider-token", "TETHER_RUN_ID": "provider-wrong"}}
			if err := rig.db.CreateSession(row, plan); err != nil {
				t.Fatal(err)
			}
			if _, err := rig.svc.launchWorkerSession(context.Background(), &row, plan); err != nil {
				t.Fatal("legacy worker launch refused", err)
			}
			spec := <-rig.specs
			if _, present := spec.Env[runtoken.TokenEnv]; present {
				t.Fatal("raw execution token sent to old/non-Linux worker")
			}
			if spec.Env[runtoken.IDEnv] == "" || spec.Env[runtoken.IDEnv] == "provider-id" || spec.Env["TETHER_RUN_ID"] != torque {
				t.Fatal("legacy worker execution/Torque ids lost")
			}
			if _, err := os.Stat(rig.svc.Store.RunSigningKeyPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsupported worker created signing material")
			}
			p, err := rig.ids.Verify(context.Background(), spec.Credential)
			if err != nil {
				t.Fatal(err)
			}
			snapshot := api.ResolveCallerContext(identity.WithPrincipal(context.Background(), p), rig.db, nil)
			if snapshot.RunVerified || snapshot.ExecutionRunID != "" || snapshot.RunID != torque {
				t.Fatal("legacy worker acquired verified execution")
			}
		})
	}
}

func testRunProof(t *testing.T, env map[string]string) string {
	t.Helper()
	token, err := runtoken.ReadProviderToken(env[runtoken.FileEnv], env[runtoken.TokenEnv], true)
	if err != nil {
		t.Fatal("run proof unavailable:", err)
	}
	return token
}

func TestRunProofExpiryAndRevocation(t *testing.T) {
	for _, target := range []string{"jti", "run"} {
		t.Run(target, func(t *testing.T) {
			svc, _, id, _ := credentialLaunch(t)
			row, err := svc.Store.GetSession(id)
			if err != nil {
				t.Fatal(err)
			}
			env, err := svc.mintRunEnvironment(context.Background(), row)
			if err != nil {
				t.Fatal(err)
			}
			p := identity.Principal{Verified: true, ID: identity.SessionPrincipalIDPrefix + id, Kind: "session", SessionID: id}
			c, err := svc.VerifyRunToken(env[runtoken.TokenEnv], p)
			if err != nil {
				t.Fatal(err)
			}
			if remaining := time.Until(time.Unix(c.Expires, 0)); remaining > 6*time.Hour || remaining < 5*time.Hour {
				t.Fatal("unbounded execution proof")
			}
			denied := c.JTI
			if target == "run" {
				denied = c.RunID
			}
			if err := svc.RevokeRunToken(context.Background(), denied); err != nil {
				t.Fatal(err)
			}
			reopened := &Service{Store: svc.Store}
			if _, err := reopened.VerifyRunToken(env[runtoken.TokenEnv], p); !errors.Is(err, runtoken.ErrInvalid) {
				t.Fatal("revoked proof accepted")
			}
			next, err := svc.mintRunEnvironment(context.Background(), row)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.VerifyRunToken(next[runtoken.TokenEnv], p); err != nil {
				t.Fatal("unrelated execution revoked")
			}
		})
	}
}

func TestRunProofUsesLifecycleDeadline(t *testing.T) {
	svc, _, id, _ := credentialLaunch(t)
	row, err := svc.Store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.lifecycle',json('{"max_duration":"20m"}')) WHERE session_id=?`, id); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	row.CreatedAt = now.Add(-24 * time.Hour).Format(time.RFC3339)
	expires, err := svc.runTokenExpiry(row, now)
	if err != nil {
		t.Fatal(err)
	}
	if !expires.Equal(now.Add(25 * time.Minute)) {
		t.Fatal("lifecycle deadline ignored")
	}
	if _, err := svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.lifecycle',json('{"max_duration":"invalid"}')) WHERE session_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.runTokenExpiry(row, now); err == nil {
		t.Fatal("invalid lifecycle silently downgraded")
	}
}

func TestLocalRunProofEnvironmentFallbackRequiresLaunchFlag(t *testing.T) {
	svc, rt, id, _ := credentialLaunch(t)
	if _, err := svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.env.TETHER_RUN_TOKEN_ENV_FALLBACK','1') WHERE session_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LaunchSession(id); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, entry := range rt.options.Env {
		k, v, _ := strings.Cut(entry, "=")
		env[k] = v
	}
	if env[runtoken.TokenEnv] == "" || env[runtoken.FileEnv] != "" || env[runtoken.EnvFallback] != "1" {
		t.Fatal("explicit compatibility flag was not honored")
	}
}

func TestOldWorkerProofDeliveryRequiresExplicitFallback(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprint(allow), func(t *testing.T) {
			rig := newIdentityRig(t, "session-cred", "agent-token", workerwire.FeatureExecutionRunToken)
			row := store.SessionRow{ID: "legacy-env-worker", WorkerID: rig.worker, LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderKind: "cli", Workspace: t.TempDir(), State: "created"}
			plan := &launch.Plan{ProviderID: "claude", ProviderBrand: "claude", Env: map[string]string{}}
			if allow {
				plan.Env[runtoken.EnvFallback] = "1"
			}
			if err := rig.db.CreateSession(row, plan); err != nil {
				t.Fatal(err)
			}
			if _, err := rig.svc.launchWorkerSession(context.Background(), &row, plan); err != nil {
				t.Fatal(err)
			}
			spec := <-rig.specs
			if (spec.Env[runtoken.TokenEnv] != "") != allow {
				t.Fatal("old worker proof delivery ignored explicit compatibility flag")
			}
		})
	}
}
