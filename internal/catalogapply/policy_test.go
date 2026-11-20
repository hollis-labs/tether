package catalogapply

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testWindowTrust struct{ key ed25519.PrivateKey }

func (t testWindowTrust) SealCatalogBreakglass(b []byte) (string, error) {
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(t.key, b)), nil
}
func (t testWindowTrust) VerifyCatalogBreakglass(b []byte, s string) error {
	sig, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !ed25519.Verify(t.key.Public().(ed25519.PublicKey), b, sig) {
		return errors.New("invalid signature")
	}
	return nil
}

const delegateURN = "msg://agent/test/helper"

func policyEngine(t *testing.T) (*Engine, *time.Time) {
	t.Helper()
	e := newEngine(t)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e.WindowTrust = testWindowTrust{key}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	e.PolicyFile = filepath.Join(t.TempDir(), "catalog-approvers.json")
	e.BreakglassFile = filepath.Join(t.TempDir(), "catalog-breakglass.json")
	setPolicy(t, e, delegateURN, PrincipalPolicy{Scopes: []string{ApproveScope}, RateLimit: 2, Allowlist: []ApprovalRule{{Name: "launch-create", Paths: []string{"launches/*.yaml"}, Actions: []string{"create"}}}})
	return e, &now
}
func setPolicy(t *testing.T, e *Engine, urn string, p PrincipalPolicy) {
	t.Helper()
	b, err := json.Marshal(ApproverPolicy{Principals: map[string]PrincipalPolicy{urn: p}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(e.PolicyFile, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func delegatePlan(t *testing.T, e *Engine, rel string) *Plan {
	t.Helper()
	p, err := e.PlanBundle(context.Background(), Bundle{Name: "test", Files: []File{{Path: rel, Content: "id: test\n"}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func delegateApprove(e *Engine, p *Plan) (*Plan, error) {
	return e.Approve(ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Delegated: true, Principal: delegateURN, AckPrivileged: true})
}
func TestApproverAllowlistRateAuditAndReload(t *testing.T) {
	e, now := policyEngine(t)
	p := delegatePlan(t, e, "launches/new.yaml")
	if _, err := delegateApprove(e, p); err != nil {
		t.Fatal(err)
	}
	history, err := e.History(20)
	if err != nil || len(history) != 1 || history[0].Principal != delegateURN || history[0].Rule != "launch-create" || history[0].Digest != p.Digest {
		t.Fatalf("audit: %#v %v", history, err)
	}
	denied := delegatePlan(t, e, "agents/new.yaml")
	if _, err := delegateApprove(e, denied); err == nil {
		t.Fatal("path outside allowlist approved")
	}
	modify := delegatePlan(t, e, "launches/general.yaml")
	if _, err := delegateApprove(e, modify); err == nil {
		t.Fatal("modify approved by create rule")
	}
	p2 := delegatePlan(t, e, "launches/next.yaml")
	if _, err := delegateApprove(e, p2); err != nil {
		t.Fatal(err)
	}
	p3 := delegatePlan(t, e, "launches/third.yaml")
	if _, err := delegateApprove(e, p3); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("rate limit: %v", err)
	}
	*now = now.Add(time.Hour)
	// Plans survive policy reload: broaden policy without replacing Engine.plans.
	setPolicy(t, e, delegateURN, PrincipalPolicy{Scopes: []string{ApproveScope}, RateLimit: 10, Allowlist: []ApprovalRule{{Name: "new-rule", Paths: []string{"agents/*.yaml"}, Actions: []string{"create"}}}})
	denied.ExpiresAt = *now // original plan remains in the engine; use fresh nonexpired plan below
	p4 := delegatePlan(t, e, "agents/fourth.yaml")
	if _, err := delegateApprove(e, p4); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.PolicyFile, []byte(`{"principals":`), 0600); err != nil {
		t.Fatal(err)
	}
	if e.DelegatedApprover(delegateURN) {
		t.Fatal("malformed policy admitted principal")
	}
	if _, err := delegateApprove(e, p4); err == nil {
		t.Fatal("malformed policy approved")
	}
	if err := os.Remove(e.PolicyFile); err != nil {
		t.Fatal(err)
	}
	e.DelegatedApps = []string{"tachyon"}
	if e.DelegatedApprover("msg://service/tachyon/prod") {
		t.Fatal("policy deletion restored fallback")
	}
}
func TestApproverFencesSurviveBreakglass(t *testing.T) {
	e, _ := policyEngine(t)
	if _, err := e.Breakglass(delegateURN, "test", time.Hour, passphrase, false); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		configure func(*Plan, *ApprovalRequest)
	}{
		{"operator-only", func(p *Plan, r *ApprovalRequest) { p.OperatorOnly = true }},
		{"privileged", func(p *Plan, r *ApprovalRequest) { p.Privileged = []string{"privileged"} }},
		{"self-config", func(p *Plan, r *ApprovalRequest) { p.Files[0].Path = "catalog-approvers.json" }},
		{"passphrase", func(p *Plan, r *ApprovalRequest) { p.Files[0].Path = "catalog-approver.json" }},
		{"trust-root", func(p *Plan, r *ApprovalRequest) { p.Files[0].Path = "global.yaml" }},
		{"own-definition", func(p *Plan, r *ApprovalRequest) { p.Files[0].Path = "agents/helper.yaml" }},
		{"protected-path", func(p *Plan, r *ApprovalRequest) { r.ProtectedPaths = []string{p.Files[0].Path} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := delegatePlan(t, e, "agents/other-"+tc.name+".yaml")
			req := ApprovalRequest{PlanID: p.ID, Digest: p.Digest, Delegated: true, Principal: delegateURN, AckPrivileged: true}
			tc.configure(e.plans[p.ID], &req)
			if _, err := e.Approve(req); err == nil {
				t.Fatal("fence bypassed")
			}
		})
	}
}
func TestBreakglassLifecycleRestartAudit(t *testing.T) {
	e, now := policyEngine(t)
	if _, err := e.Breakglass(delegateURN, "test", time.Hour, "wrong", false); err == nil {
		t.Fatal("wrong passphrase opened window")
	}
	for _, d := range []time.Duration{-time.Second, 0, 5 * time.Hour} {
		if _, err := e.Breakglass(delegateURN, "test", d, passphrase, false); err == nil {
			t.Fatal("invalid duration")
		}
	}
	if _, err := e.Breakglass("msg://service/unknown", "test", time.Hour, passphrase, false); err == nil {
		t.Fatal("unknown principal opened")
	}
	if _, err := e.Breakglass(delegateURN, "test", time.Hour, passphrase, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Breakglass(delegateURN, "test", time.Hour, passphrase, false); err == nil {
		t.Fatal("overlapping windows")
	}
	p := delegatePlan(t, e, "agents/outside.yaml")
	if _, err := delegateApprove(e, p); err != nil {
		t.Fatal(err)
	}
	// Restart in a fresh engine with the same isolated operator trust file.
	next := newEngine(t)
	next.Approver = e.Approver
	next.WindowTrust = e.WindowTrust
	next.PolicyFile = e.PolicyFile
	next.BreakglassFile = e.BreakglassFile
	next.Now = e.Now
	view, err := next.Approvers()
	if err != nil || view.Breakglass == nil {
		t.Fatalf("restore: %#v %v", view, err)
	}
	q := delegatePlan(t, next, "agents/restarted.yaml")
	if _, err := delegateApprove(next, q); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	view, err = next.Approvers()
	if err != nil || view.Breakglass != nil {
		t.Fatalf("expiry: %#v %v", view, err)
	}
	q = delegatePlan(t, next, "agents/expired.yaml")
	if _, err := delegateApprove(next, q); err == nil {
		t.Fatal("expired window used")
	}
	if _, err := next.Breakglass(delegateURN, "another", time.Hour, passphrase, false); err != nil {
		t.Fatal(err)
	}
	if _, err := next.Breakglass("", "", 0, passphrase, true); err != nil {
		t.Fatal(err)
	}
	q = delegatePlan(t, next, "agents/ended.yaml")
	if _, err := delegateApprove(next, q); err == nil {
		t.Fatal("ended window used")
	}
	records, err := next.History(20)
	if err != nil {
		t.Fatal(err)
	}
	events := map[string]bool{}
	for _, r := range records {
		events[r.Event] = true
	}
	for _, event := range []string{"breakglass_open", "breakglass_expire", "breakglass_end", "delegate_approve"} {
		if !events[event] {
			t.Errorf("missing audit %s", event)
		}
	}
}
func TestBreakglassFailsClosed(t *testing.T) {
	for _, tc := range []string{"parse", "tamper", "clock", "policy", "rotation"} {
		t.Run(tc, func(t *testing.T) {
			e, now := policyEngine(t)
			if _, err := e.Breakglass(delegateURN, "test", time.Hour, passphrase, false); err != nil {
				t.Fatal(err)
			}
			switch tc {
			case "parse":
				if err := os.WriteFile(e.BreakglassFile, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "tamper":
				b, err := os.ReadFile(e.BreakglassFile)
				if err != nil {
					t.Fatal(err)
				}
				b = []byte(strings.Replace(string(b), "test", "evil", 1))
				if err = os.WriteFile(e.BreakglassFile, b, 0600); err != nil {
					t.Fatal(err)
				}
			case "clock":
				*now = now.Add(-time.Second)
			case "policy":
				setPolicy(t, e, delegateURN, PrincipalPolicy{Scopes: []string{ApproveScope}, RateLimit: 3})
			case "rotation":
				if err := e.Approver.Set(passphrase, "new correct horse battery"); err != nil {
					t.Fatal(err)
				}
			}
			e.windowLoaded = false
			e.window = nil
			for i := 0; i < 2; i++ {
				p := delegatePlan(t, e, "agents/refused.yaml")
				if _, err := delegateApprove(e, p); err == nil {
					t.Fatal("invalid restart state approved")
				}
			}
		})
	}
}

func TestApproverExplicitPrivilegeAndBaseBinding(t *testing.T) {
	e, _ := policyEngine(t)
	setPolicy(t, e, delegateURN, PrincipalPolicy{Scopes: []string{ApproveScope}, RateLimit: 10, Privileged: true, Allowlist: []ApprovalRule{{Name: "agent-modify", Paths: []string{"agents/*.yaml"}, Actions: []string{"modify"}}}})
	p, err := e.PlanBundle(context.Background(), Bundle{Name: "privileged", Files: []File{{Path: "agents/general.yaml", Content: "id: general\npermissions:\n  permission_mode: bypass\n"}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Privileged) == 0 {
		t.Fatal("fixture is not privileged")
	}
	if _, err := delegateApprove(e, p); err != nil {
		t.Fatalf("explicit permission: %v", err)
	}
	q := delegatePlan(t, e, "agents/general.yaml")
	write(t, e.Root, "agents/general.yaml", "id: changed-live\n")
	if _, err := delegateApprove(e, q); err == nil {
		t.Fatal("stale live base approved")
	}
}
func TestBreakglassTimerExpiresWithoutRequest(t *testing.T) {
	e, _ := policyEngine(t)
	e.Now = time.Now
	if _, err := e.Breakglass(delegateURN, "timer test", 30*time.Millisecond, passphrase, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		closed := e.window == nil
		e.mu.Unlock()
		if closed {
			records, err := e.History(20)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range records {
				if r.Event == "breakglass_expire" {
					return
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timer did not close and audit window")
}

func TestBreakglassRejectsChangedIssuerInRunningProcess(t *testing.T) {
	e, _ := policyEngine(t)
	if _, err := e.Breakglass(delegateURN, "rotation", time.Hour, passphrase, false); err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e.WindowTrust = testWindowTrust{key}
	p := delegatePlan(t, e, "agents/rotated.yaml")
	if _, err := delegateApprove(e, p); err == nil {
		t.Fatal("running process resealed window with changed issuer")
	}
}
