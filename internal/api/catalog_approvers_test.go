package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/authissuer"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/catalogapply"
	"github.com/hollis-labs/tether/internal/identity"
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
		{"scoped-agent", "agent", urn, []string{"catalog.approve"}, true, 200},
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
