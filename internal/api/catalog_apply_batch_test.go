package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/catalogapply"
)

func TestCatalogBatchApprovalHappyPathAndPartialFailure(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	p := d.plan(helperFiles, asSession)
	q := d.plan([]catalogapply.File{{Path: "agents/other.yaml", Content: "id: other\nname: Other\n"}}, asSession)
	req := api.CatalogApproveBatchRequest{Plans: []catalogapply.ApprovalItem{{ID: p.ID, Digest: p.Digest}, {ID: q.ID, Digest: q.Digest}}, Passphrase: applyPass}
	code, b := d.call("POST", "/catalog/plans/approve-batch", req, asOperator)
	if code != http.StatusOK {
		t.Fatalf("batch: %d %s", code, b)
	}
	var res api.CatalogApproveBatchResponse
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 2 {
		t.Fatalf("results: %s", b)
	}
	for _, r := range res.Results {
		if r.Error != "" || r.Plan == nil || r.Plan.State != catalogapply.StateApproved {
			t.Fatalf("result: %+v", r)
		}
	}
	// Mixed failures leave the valid plan approved and each failure unapproved.
	p = d.plan([]catalogapply.File{{Path: "agents/third.yaml", Content: "id: third\n"}}, asSession)
	q = d.plan([]catalogapply.File{{Path: "agents/fourth.yaml", Content: "id: fourth\n"}}, asSession)
	req.Plans = []catalogapply.ApprovalItem{{ID: p.ID, Digest: p.Digest}, {ID: q.ID, Digest: "bad"}, {ID: "missing", Digest: q.Digest}}
	code, b = d.call("POST", "/catalog/plans/approve-batch", req, asOperator)
	if code != http.StatusOK {
		t.Fatalf("partial batch: %d %s", code, b)
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 3 || res.Results[0].Plan.State != catalogapply.StateApproved || res.Results[1].Error == "" || res.Results[2].Error == "" {
		t.Fatalf("partial response: %s", b)
	}
	got, _ := d.engine.Get(q.ID)
	if got.State != catalogapply.StatePlanned {
		t.Fatal("failure approved")
	}
}

func TestCatalogBatchApprovalGateAndLockout(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	p := d.plan(helperFiles, asSession)
	req := api.CatalogApproveBatchRequest{Plans: []catalogapply.ApprovalItem{{ID: p.ID, Digest: p.Digest}}, Passphrase: applyPass}
	for name, h := range map[string]map[string]string{
		"session": asSession, "bare-session": asBareSession,
		"device":           {"X-Test-Principal": "device"},
		"agent":            {"X-Test-Principal": "agent"},
		"other-service":    {"X-Test-Principal": "service"},
		"unscoped-service": {"X-Test-Principal": "unscoped-service"},
		"remote-operator":  {"X-Test-Principal": "operator", "X-Test-Remote": "1"},
	} {
		code, b := d.call("POST", "/catalog/plans/approve-batch", req, h)
		if code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, code, b)
		}
	}
	// Four wrong batches of two plans must NOT lock out a valid fifth batch.
	q := d.plan([]catalogapply.File{{Path: "agents/other.yaml", Content: "id: other\n"}}, asSession)
	req.Plans = append(req.Plans, catalogapply.ApprovalItem{ID: q.ID, Digest: q.Digest})
	req.Passphrase = "wrong"
	for i := 0; i < 4; i++ {
		if code, b := d.call("POST", "/catalog/plans/approve-batch", req, asOperator); code != 403 {
			t.Fatalf("wrong batch: %d %s", code, b)
		}
	}
	req.Passphrase = applyPass
	if code, b := d.call("POST", "/catalog/plans/approve-batch", req, asOperator); code != 200 {
		t.Fatalf("counted more than once per batch: %d %s", code, b)
	}
}

func TestCatalogBatchMethodsShapeAndTCPGate(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	if code, _ := d.call("GET", "/catalog/plans/approve-batch", nil, asOperator); code != 405 {
		t.Fatalf("method: %d", code)
	}
	for _, req := range []api.CatalogApproveBatchRequest{{}, {Plans: []catalogapply.ApprovalItem{{ID: "x"}, {ID: "x"}}}} {
		if code, b := d.call("POST", "/catalog/plans/approve-batch", req, asOperator); code != 400 {
			t.Fatalf("shape: %d %s", code, b)
		}
	}
	// The route gate also rejects requests that have no marked local connection.
	h := api.NewHandler(api.Deps{CatalogApply: d.engine})
	req := httptest.NewRequest(http.MethodPost, "/catalog/plans/approve-batch", bytes.NewBufferString(`{"plans":[{"id":"x","digest":"x"}],"passphrase":"irrelevant"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("unmarked connection bypass: %d %s", w.Code, w.Body.String())
	}
}

func TestCatalogPlanHTTPShowsRestartAndExpiry(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	d.engine.PlansFile = t.TempDir() + "/plans.json"
	p := d.plan(helperFiles, asSession)
	code, b := d.call("GET", "/catalog/plans/"+p.ID, nil, asSession)
	var got catalogapply.Plan
	if code != 200 || json.Unmarshal(b, &got) != nil || !got.SurvivesRestart || got.ExpiresAt.IsZero() || got.ExpiresAt.Before(time.Now()) {
		t.Fatalf("get: %d %s", code, b)
	}
}

func TestCatalogBatchDelegatedServicePreservesPrivilegeFences(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	d.engine.DelegatedApps = []string{"tachyon"}
	normal := d.plan(helperFiles, asSession)
	privileged := d.plan([]catalogapply.File{{Path: "agents/privileged.yaml", Content: "id: privileged\npermissions: [all]\n"}}, asSession)
	trust := d.plan([]catalogapply.File{{Path: "global.yaml", Content: "version: 1\noperator_attestation:\n  key_id: next\n"}}, asSession)
	req := api.CatalogApproveBatchRequest{Plans: []catalogapply.ApprovalItem{{ID: normal.ID, Digest: normal.Digest}, {ID: privileged.ID, Digest: privileged.Digest}, {ID: trust.ID, Digest: trust.Digest}}, AckPrivileged: true}
	code, b := d.call("POST", "/catalog/plans/approve-batch", req, map[string]string{"X-Test-Principal": "delegated-service"})
	var res api.CatalogApproveBatchResponse
	if code != 200 || json.Unmarshal(b, &res) != nil || len(res.Results) != 3 || res.Results[0].Plan == nil || res.Results[1].Error == "" || res.Results[2].Error == "" {
		t.Fatalf("delegated batch: %d %s", code, b)
	}
	d.engine.DelegatedPrivileged = true
	code, b = d.call("POST", "/catalog/plans/approve-batch", req, map[string]string{"X-Test-Principal": "delegated-service"})
	if code != 200 || json.Unmarshal(b, &res) != nil || res.Results[1].Plan == nil || res.Results[2].Error == "" {
		t.Fatalf("delegated privileged batch: %d %s", code, b)
	}
	got, _ := d.engine.Get(trust.ID)
	if got.State != catalogapply.StatePlanned {
		t.Fatal("delegated trust-root plan approved")
	}
}
