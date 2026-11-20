package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/catalogapply"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func TestCatalogConfiguredApproverAdmission(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	urn := "msg://service/custom/dev"
	d.engine.PolicyFile = filepath.Join(t.TempDir(), "catalog-approvers.json")
	b, err := json.Marshal(catalogapply.ApproverPolicy{Principals: map[string]catalogapply.PrincipalPolicy{urn: {Scopes: []string{catalogapply.ApproveScope}, RateLimit: 10, Allowlist: []catalogapply.ApprovalRule{{Name: "agents-create", Paths: []string{"agents/*.yaml"}, Actions: []string{"create"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(d.engine.PolicyFile, b, 0600); err != nil {
		t.Fatal(err)
	}
	h := api.AgentScopeMiddleware(identity.Enforce, api.NewHandler(api.Deps{CatalogApply: d.engine}))
	for _, tc := range []struct {
		name, kind, id string
		scopes         []string
		verified       bool
		want           int
	}{
		{"scoped-service", "service", urn, []string{"catalog.approve"}, true, 200},
		{"forged-agent-kind", "agent", urn, []string{"catalog.approve"}, true, 403},
		{"write-is-not-approve", "service", urn, []string{"catalog.write"}, true, 403},
		{"maintain-is-not-approve", "service", urn, []string{"maintain"}, true, 403},
		{"unlisted", "service", "msg://service/else/dev", []string{"catalog.approve"}, true, 403},
		{"unverified", "service", urn, []string{"catalog.approve"}, false, 403},
		{"session", "session", urn, []string{"catalog.approve"}, true, 403},
		{"device", "device", urn, []string{"catalog.approve"}, true, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := d.engine.PlanBundle(context.Background(), catalogapply.Bundle{Name: tc.name, Files: []catalogapply.File{{Path: "agents/" + tc.name + ".yaml", Content: "id: example\n"}}}, catalogapply.Options{})
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(api.CatalogApproveRequest{Digest: p.Digest})
			r := httptest.NewRequest(http.MethodPost, "/catalog/plans/"+p.ID+"/approve", bytes.NewReader(body))
			r = r.WithContext(identity.WithPrincipal(r.Context(), identity.Principal{Verified: tc.verified, Kind: tc.kind, ID: tc.id, TokenID: "issued", Scopes: tc.scopes}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestCatalogBreakglassOperatorAdmission(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	for _, who := range []string{"session", "agent", "device", "delegated-service", "service"} {
		if status, _ := d.call("POST", "/catalog/breakglass", api.CatalogBreakglassRequest{Principal: "msg://service/tachyon/prod", For: "1h", Reason: "test", Passphrase: applyPass}, map[string]string{"X-Test-Principal": who}); status != 403 {
			t.Fatalf("%s status %d", who, status)
		}
	}
	if status, _ := d.call("POST", "/catalog/breakglass", api.CatalogBreakglassRequest{Passphrase: applyPass}, map[string]string{"X-Test-Principal": "operator", "X-Test-Remote": "1"}); status != 403 {
		t.Fatalf("remote operator status %d", status)
	}
}

func TestCatalogBreakglassLocalOperatorLifecycle(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	d.engine.DelegatedApps = []string{"tachyon"}
	d.engine.BreakglassFile = filepath.Join(t.TempDir(), "catalog-breakglass.json")
	keys, err := authissuer.OpenKeyStore(filepath.Join(t.TempDir(), "auth"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	d.engine.WindowTrust = keys
	req := api.CatalogBreakglassRequest{Principal: "msg://service/tachyon/prod", For: "1h", Reason: "fixture", Passphrase: applyPass}
	headers := map[string]string{"X-Test-Principal": "operator"}
	if code, b := d.call("POST", "/catalog/breakglass", req, headers); code != 200 {
		t.Fatalf("open %d %s", code, b)
	}
	if code, b := d.call("GET", "/catalog/approvers", nil, nil); code != 200 || !bytes.Contains(b, []byte("msg://service/tachyon/prod")) {
		t.Fatalf("view %d %s", code, b)
	}
	req.End = true
	req.Passphrase = "wrong"
	if code, _ := d.call("POST", "/catalog/breakglass", req, headers); code != 403 {
		t.Fatalf("wrong passphrase end %d", code)
	}
	req.Passphrase = applyPass
	if code, b := d.call("POST", "/catalog/breakglass", req, headers); code != 200 {
		t.Fatalf("end %d %s", code, b)
	}
}

func TestCatalogMintedAgentApprovalBinding(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	urn := "msg://agent/agent-mux/agt_approver"
	dir := t.TempDir()
	d.engine.PolicyFile = filepath.Join(dir, "catalog-approvers.json")
	policy, _ := json.Marshal(catalogapply.ApproverPolicy{Principals: map[string]catalogapply.PrincipalPolicy{urn: {Scopes: []string{catalogapply.ApproveScope}, RateLimit: 10, Allowlist: []catalogapply.ApprovalRule{{Name: "agents", Paths: []string{"agents/*.yaml"}, Actions: []string{"create", "modify"}}}}}})
	if err := os.WriteFile(d.engine.PolicyFile, policy, 0600); err != nil {
		t.Fatal(err)
	}
	keys, err := authissuer.OpenKeyStore(filepath.Join(dir, "auth"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	state, err := authissuer.OpenState(filepath.Join(dir, "issuer.json"))
	if err != nil {
		t.Fatal(err)
	}
	issuer := authissuer.New("tether", keys, state)
	tok, err := issuer.Exchange(identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}, authissuer.Request{Audience: "tether", Scopes: []string{"catalog.approve"}, SessionID: "test-session"})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := authissuer.NewVerifier(nil, issuer)
	if err != nil {
		t.Fatal(err)
	}
	cat := &config.Catalog{Agents: map[string]config.Agent{"helper": {ID: "helper", URN: urn}}}
	sessions := sessionLookup{rows: map[string]store.SessionRow{"test-session": {LogicalAgentID: "helper", LaunchID: "helper-launch"}}}
	handler := api.NewHandler(api.Deps{CatalogApply: d.engine, Service: sessions, Catalog: approverCatalog{cat}})
	handler = identity.Middleware(identity.Enforce, verifier, nil, api.AgentScopeMiddleware(identity.Enforce, handler))
	request := func(rel string, remote bool) int {
		t.Helper()
		plan, err := d.engine.PlanBundle(context.Background(), catalogapply.Bundle{Name: "bound", Files: []catalogapply.File{{Path: rel, Content: "id: test\n"}}}, catalogapply.Options{})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(api.CatalogApproveRequest{Digest: plan.Digest})
		req := httptest.NewRequest("POST", "/catalog/plans/"+plan.ID+"/approve", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		if remote {
			req = req.WithContext(identity.WithRemoteContext(req.Context()))
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out.Code
	}
	if code := request("agents/other.yaml", false); code != 200 {
		t.Fatalf("real agent token refused: %d", code)
	}
	if code := request("agents/helper.yaml", false); code == 200 {
		t.Fatal("agent approved own definition")
	}
	if code := request("agents/remote.yaml", true); code != 403 {
		t.Fatalf("remote agent approval: %d", code)
	}
	cat.Agents["helper"] = config.Agent{ID: "helper", URN: "msg://agent/agent-mux/else"}
	if code := request("agents/changed-binding.yaml", false); code != 403 {
		t.Fatalf("stale seat binding: %d", code)
	}
	delete(sessions.rows, "test-session")
	if code := request("agents/no-session.yaml", false); code != 403 {
		t.Fatalf("missing session: %d", code)
	}
}

func TestCatalogRemoteApprovalRouteRemainsLocalCustody(t *testing.T) {
	req := httptest.NewRequest("POST", "/catalog/plans/example/approve", nil)
	scope, _, known := api.RequiredRemoteScope(req)
	if !known || scope != "local-only" {
		t.Fatalf("remote scope widened: %q", scope)
	}
}

type approverCatalog struct{ cat *config.Catalog }

func (a approverCatalog) Load() (*config.Catalog, error) { return a.cat, nil }
