package catalogapply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const passphrase = "correct horse battery"

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "catalog")
	write(t, root, "global.yaml", "version: 1\n")
	write(t, root, "agents/general.yaml", "id: general\nname: General\n")
	write(t, root, "launches/general.yaml", "id: general\nagent: general\n")
	write(t, root, "credentials/token.txt", "never-touched")
	ap := NewApprover(filepath.Join(dir, "state", "approver.json"))
	if err := ap.Set("", passphrase); err != nil {
		t.Fatal(err)
	}
	return &Engine{
		Root: root, BackupRoot: filepath.Join(dir, "backups"), Approver: ap,
		// A tree is invalid when any file in it says INVALID.
		Validate: func(r string) error {
			var bad bool
			_ = filepath.WalkDir(r, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if b, _ := os.ReadFile(p); strings.Contains(string(b), "INVALID") {
						bad = true
					}
				}
				return nil
			})
			if bad {
				return errors.New("tree says INVALID")
			}
			return nil
		},
	}
}

func approve(t *testing.T, e *Engine, p *Plan) {
	t.Helper()
	if _, err := e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: passphrase, AckPrivileged: true, AllowRunning: true}); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

func TestValidatePath(t *testing.T) {
	good := []string{"agents/a.yaml", "launches/x-y.yml", "skills/adr.md", "global.yaml", "teams-policy.yaml", "boot-profiles/p.yaml"}
	bad := []string{"", "/etc/passwd", "../x.yaml", "agents/../global.yaml", "agents//a.yaml", "agents/a.sh", "credentials/t.yaml",
		"agents/.hidden.yaml", "agents/a/b/c/d.yaml", "main.yaml", "agents\\a.yaml", "agents/a\x00.yaml", "~/x.yaml", "agents/", "other/a.yaml"}
	for _, p := range good {
		if err := ValidatePath(p); err != nil {
			t.Errorf("%q should be allowed: %v", p, err)
		}
	}
	for _, p := range bad {
		if err := ValidatePath(p); err == nil {
			t.Errorf("%q should be refused", p)
		}
	}
}

func TestScanSecrets(t *testing.T) {
	for _, c := range []string{
		"api_key: sk-abcdefghijklmnopqrstuvwxyz0123",
		"-----BEGIN RSA PRIVATE KEY-----\nabc",
		"env:\n  GITHUB_TOKEN: abcdefgh12345678",
		"password: hunter2hunter2",
	} {
		if ScanSecrets(c) == "" {
			t.Errorf("secret not found in %q", c)
		}
	}
	for _, c := range []string{
		"token: ${GITHUB_TOKEN}",
		"token_file: /var/lib/x/token",
		"name: Agent\nrole: reader",
		"password: short",
	} {
		if why := ScanSecrets(c); why != "" {
			t.Errorf("false positive in %q: %s", c, why)
		}
	}
}

func TestPlanApplyRollback(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	p, err := e.PlanBundle(ctx, Bundle{Name: "new-agent", Files: []File{
		{Path: "agents/helper.yaml", Content: "id: helper\nname: Helper\n"},
		{Path: "launches/helper.yaml", Content: "id: helper\nagent: helper\n"},
	}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StatePlanned || !p.ValidationOK || p.Digest == "" || !strings.Contains(p.Diff, "+id: helper") {
		t.Fatalf("plan = %+v", p)
	}
	if _, err := os.Stat(filepath.Join(e.Root, "agents/helper.yaml")); err == nil {
		t.Fatal("planning wrote to the live catalog")
	}
	if _, err := e.Apply(ctx, p.ID); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("apply before approval: %v", err)
	}
	approve(t, e, p)
	res, err := e.Apply(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.Activation != ActivationRestartRequired || len(res.Changed) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if read(t, e.Root, "agents/helper.yaml") != "id: helper\nname: Helper\n" {
		t.Fatal("agent not installed")
	}
	if read(t, e.Root, "credentials/token.txt") != "never-touched" {
		t.Fatal("credentials changed")
	}
	if got := read(t, e.BackupRoot, res.BackupID+"/agents/general.yaml"); got == "" {
		t.Fatal("backup missing")
	}
	if _, err := e.Apply(ctx, p.ID); err == nil || !strings.Contains(err.Error(), "already applied") {
		t.Fatalf("second apply: %v", err)
	}

	// Roll back to the backup: the new files go away.
	rp, err := e.PlanRollback(ctx, res.BackupID, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rp.RollbackOf != res.BackupID || len(rp.Files) != 2 {
		t.Fatalf("rollback plan = %+v", rp)
	}
	if _, err := e.Apply(ctx, rp.ID); err == nil {
		t.Fatal("rollback applied without approval")
	}
	approve(t, e, rp)
	if _, err := e.Apply(ctx, rp.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.Root, "agents/helper.yaml")); err == nil {
		t.Fatal("rollback left the agent")
	}
	if bs, _ := e.Backups(); len(bs) != 2 {
		t.Fatalf("backups = %v", bs)
	}
}

func TestLaunchOnlyChangeIsImmediate(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	p, err := e.PlanBundle(ctx, Bundle{Name: "tweak", Files: []File{{Path: "launches/general.yaml", Content: "id: general\nagent: general\nchat: true\n"}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	approve(t, e, p)
	res, err := e.Apply(ctx, p.ID)
	if err != nil || res.Activation != ActivationImmediate {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestApprovalGate(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	p, _ := e.PlanBundle(ctx, Bundle{Name: "x", Files: []File{{Path: "agents/x.yaml", Content: "id: x\n"}}}, Options{})

	cases := []struct {
		name string
		req  ApprovalRequest
		want string
	}{
		{"wrong digest", ApprovalRequest{PlanID: p.ID, Digest: "deadbeef", Passphrase: passphrase}, "digest mismatch"},
		{"wrong passphrase", ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: "not the passphrase"}, "passphrase is wrong"},
		{"no plan", ApprovalRequest{PlanID: "cp_nope", Digest: p.Digest, Passphrase: passphrase}, "not found"},
	}
	for _, c := range cases {
		if _, err := e.Approve(c.req); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	if q, _ := e.Get(p.ID); q.State != StatePlanned {
		t.Fatalf("a refused approval changed state to %s", q.State)
	}
	if _, err := e.Apply(ctx, p.ID); err == nil {
		t.Fatal("apply without approval succeeded")
	}
}

func TestNoPassphraseRefusesApproval(t *testing.T) {
	e := newEngine(t)
	e.Approver = NewApprover(filepath.Join(t.TempDir(), "none.json"))
	p, _ := e.PlanBundle(context.Background(), Bundle{Name: "x", Files: []File{{Path: "agents/x.yaml", Content: "id: x\n"}}}, Options{})
	if _, err := e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: ""}); !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("err = %v", err)
	}
}

func TestLockoutAfterRepeatedFailures(t *testing.T) {
	e := newEngine(t)
	now := time.Now()
	e.Approver.now = func() time.Time { return now }
	p, _ := e.PlanBundle(context.Background(), Bundle{Name: "x", Files: []File{{Path: "agents/x.yaml", Content: "id: x\n"}}}, Options{})
	for i := 0; i < maxFailures; i++ {
		_, _ = e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: "wrong wrong wrong"})
	}
	if _, err := e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: passphrase}); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("expected lockout, got %v", err)
	}
	now = now.Add(lockoutDuration + time.Second)
	if _, err := e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: passphrase}); err != nil {
		t.Fatalf("after lockout: %v", err)
	}
}

func TestPrivilegedNeedsAck(t *testing.T) {
	e := newEngine(t)
	p, err := e.PlanBundle(context.Background(), Bundle{Name: "perm", Files: []File{
		{Path: "agents/general.yaml", Content: "id: general\nname: General\npermissions:\n  permission_mode: bypass\n"},
	}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Privileged) == 0 || p.ApprovalRequired == "" {
		t.Fatalf("permission change not flagged: %+v", p)
	}
	if _, err := e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: passphrase}); err == nil || !strings.Contains(err.Error(), "privileged") {
		t.Fatalf("approved without ack: %v", err)
	}
	if _, err := e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: passphrase, AckPrivileged: true}); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedPathRefused(t *testing.T) {
	e := newEngine(t)
	_, err := e.PlanBundle(context.Background(), Bundle{Name: "self", Files: []File{{Path: "agents/general.yaml", Content: "id: general\n"}}},
		Options{ProtectedPaths: []string{"agents/general.yaml"}})
	if err == nil || !strings.Contains(err.Error(), "own definition") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefusals(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	for name, b := range map[string]Bundle{
		"bad path":     {Name: "b", Files: []File{{Path: "../x.yaml", Content: "a: b\n"}}},
		"credentials":  {Name: "b", Files: []File{{Path: "credentials/t.yaml", Content: "a: b\n"}}},
		"secret":       {Name: "b", Files: []File{{Path: "agents/s.yaml", Content: "api_key: sk-abcdefghijklmnopqrstuvwxyz0123\n"}}},
		"empty":        {Name: "b"},
		"bad name":     {Name: "../b", Files: []File{{Path: "agents/s.yaml", Content: "a: b\n"}}},
		"dup":          {Name: "b", Files: []File{{Path: "agents/s.yaml", Content: "a: b\n"}, {Path: "agents/s.yaml", Content: "a: c\n"}}},
		"delete none":  {Name: "b", Files: []File{{Path: "agents/none.yaml", Delete: true}}},
		"empty body":   {Name: "b", Files: []File{{Path: "agents/s.yaml"}}},
		"delete+body":  {Name: "b", Files: []File{{Path: "agents/general.yaml", Delete: true, Content: "x"}}},
		"outside root": {Name: "b", Files: []File{{Path: "global.yaml/../../etc.yaml", Content: "a: b\n"}}},
	} {
		if _, err := e.PlanBundle(ctx, b, Options{}); err == nil {
			t.Errorf("%s: plan accepted", name)
		}
	}
	// A symlinked destination is not written through.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.Root, "skills")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlanBundle(ctx, Bundle{Name: "b", Files: []File{{Path: "skills/x.md", Content: "hi\n"}}}, Options{}); err == nil {
		t.Error("symlinked directory accepted")
	}
}

func TestInvalidPlanCannotBeApproved(t *testing.T) {
	e := newEngine(t)
	p, err := e.PlanBundle(context.Background(), Bundle{Name: "bad", Files: []File{{Path: "agents/bad.yaml", Content: "INVALID\n"}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.State != StateInvalid || p.ValidationError == "" {
		t.Fatalf("plan = %+v", p)
	}
	if _, err := e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: passphrase}); err == nil {
		t.Fatal("invalid plan approved")
	}
}

func TestNoOpPlanIsInvalid(t *testing.T) {
	e := newEngine(t)
	p, err := e.PlanBundle(context.Background(), Bundle{Name: "noop", Files: []File{{Path: "agents/general.yaml", Content: "id: general\nname: General\n"}}}, Options{})
	if err != nil || p.State != StateInvalid {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestStalePlanRefused(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	p, _ := e.PlanBundle(ctx, Bundle{Name: "m", Files: []File{{Path: "agents/general.yaml", Content: "id: general\nname: Changed\n"}}}, Options{})
	approve(t, e, p)
	write(t, e.Root, "agents/general.yaml", "id: general\nname: Edited elsewhere\n")
	if _, err := e.Apply(ctx, p.ID); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("err = %v", err)
	}
}

func TestPostInstallValidationFailureRestores(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	p, _ := e.PlanBundle(ctx, Bundle{Name: "m", Files: []File{{Path: "agents/general.yaml", Content: "id: general\nname: Changed\n"}}}, Options{})
	approve(t, e, p)
	calls := 0
	e.Validate = func(string) error {
		calls++
		return errors.New("health failed")
	}
	res, err := e.Apply(ctx, p.ID)
	if err == nil || res == nil || !res.RolledBack {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if read(t, e.Root, "agents/general.yaml") != "id: general\nname: General\n" {
		t.Fatal("original not restored")
	}
	if calls != 1 {
		t.Fatalf("validate calls = %d", calls)
	}
}

func TestRunningSessionsBlockUnlessAllowed(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	e.RunningSessions = func(_ context.Context, touches []string) ([]string, error) {
		for _, t := range touches {
			if t == "agent:general" {
				return []string{"sess-1"}, nil
			}
		}
		return nil, nil
	}
	p, _ := e.PlanBundle(ctx, Bundle{Name: "m", Files: []File{{Path: "agents/general.yaml", Content: "id: general\nname: Changed\n"}}}, Options{})
	if len(p.RunningSessions) != 1 {
		t.Fatalf("plan = %+v", p)
	}
	if _, err := e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: passphrase}); err == nil {
		t.Fatal("approved over a running session without allowing it")
	}
	if _, err := e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: passphrase, AllowRunning: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPlanExpires(t *testing.T) {
	e := newEngine(t)
	now := time.Now()
	e.Now = func() time.Time { return now }
	p, _ := e.PlanBundle(context.Background(), Bundle{Name: "x", Files: []File{{Path: "agents/x.yaml", Content: "id: x\n"}}}, Options{})
	now = now.Add(time.Hour)
	if _, err := e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Passphrase: passphrase}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v", err)
	}
}

func TestPassphraseRules(t *testing.T) {
	a := NewApprover(filepath.Join(t.TempDir(), "a.json"))
	if err := a.Set("", "short"); err == nil {
		t.Fatal("short passphrase accepted")
	}
	if err := a.Set("", passphrase); err != nil {
		t.Fatal(err)
	}
	if err := a.Set("wrong", "another long passphrase"); err == nil {
		t.Fatal("changed without the current passphrase")
	}
	if err := a.Set(passphrase, "another long passphrase"); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(passphrase); err == nil {
		t.Fatal("old passphrase still valid")
	}
	fi, _ := os.Stat(a.path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	if b, _ := os.ReadFile(a.path); strings.Contains(string(b), "another long") {
		t.Fatal("passphrase stored in the clear")
	}
}

func TestDiff(t *testing.T) {
	d := unifiedDiff("agents/a.yaml", "a\nb\nc\nd\ne\nf\ng\nh\n", "a\nb\nX\nd\ne\nf\ng\nh\ni\n")
	for _, want := range []string{"--- a/agents/a.yaml", "+++ b/agents/a.yaml", "-c", "+X", "+i"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
	if unifiedDiff("x", "same\n", "same\n") != "" {
		t.Error("identical files produced a diff")
	}
	if c := unifiedDiff("n", "", "new\n"); !strings.Contains(c, "--- /dev/null") {
		t.Errorf("create diff: %s", c)
	}
}
