package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/runtoken"
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
	if !snap.Verified || !snap.SessionResolved || !snap.RunVerified || snap.RunID != c.RunID || snap.TokenJTI != c.JTI {
		t.Fatal("run attribution not additive")
	}
	for _, mutate := range []func(*identity.Principal){func(p *identity.Principal) { p.SessionID = "other" }, func(p *identity.Principal) { p.Verified = false }, func(p *identity.Principal) { p.Kind = "service" }} {
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
