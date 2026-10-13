package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/catalogapply"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

const applyPass = "correct horse battery"

type sessionLookup struct {
	api.LaunchService
	rows map[string]store.SessionRow
}

func (s sessionLookup) GetSession(id string) (*store.SessionRow, error) {
	r, ok := s.rows[id]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

// catalogApplyDaemon serves the catalog routes on a real unix socket, the way
// the daemon does, with a stand-in for the identity middleware that takes the
// principal from a test header.
type catalogApplyDaemon struct {
	t      *testing.T
	root   string
	http   *http.Client
	engine *catalogapply.Engine
}

func newCatalogApplyDaemon(t *testing.T) *catalogApplyDaemon {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "catalog")
	for rel, body := range map[string]string{
		"global.yaml":          "version: 1\n",
		"agents/general.yaml":  "id: general\nname: General\n",
		"agents/manager.yaml":  "id: manager\nname: Manager\n",
		"launches/manager.yml": "id: manager\nagent: manager\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ap := catalogapply.NewApprover(filepath.Join(dir, "approver.json"))
	if err := ap.Set("", applyPass); err != nil {
		t.Fatal(err)
	}
	eng := &catalogapply.Engine{Root: root, BackupRoot: filepath.Join(dir, "backups"), Approver: ap, Validate: func(string) error { return nil }}
	svc := sessionLookup{rows: map[string]store.SessionRow{
		"sess-mgr": {ID: "sess-mgr", LogicalAgentID: "manager", LaunchID: "manager"},
	}}
	inner := api.NewHandler(api.Deps{CatalogApply: eng, Service: svc})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Test-Principal") {
		case "session":
			scopes := []string{"message.write"}
			if r.Header.Get("X-Test-Scope") != "" {
				scopes = append(scopes, r.Header.Get("X-Test-Scope"))
			}
			r = r.WithContext(identity.WithPrincipal(r.Context(), identity.Principal{Verified: true, ID: "sess-mgr", Kind: "session", SessionID: "sess-mgr", Scopes: scopes}))
		case "operator":
			r = r.WithContext(identity.WithPrincipal(r.Context(), identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}))
		}
		if r.Header.Get("X-Test-Remote") != "" {
			r = r.WithContext(identity.WithRemoteContext(r.Context()))
		}
		inner.ServeHTTP(w, r)
	})
	sock := filepath.Join(os.TempDir(), "tcad-"+filepath.Base(dir)+".sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h, ConnContext: identity.ConnectionContext}}
	srv.Start()
	t.Cleanup(func() { srv.Close(); _ = os.Remove(sock) })
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	return &catalogApplyDaemon{t: t, root: root, http: c, engine: eng}
}

func (d *catalogApplyDaemon) call(method, path string, body any, headers map[string]string) (int, []byte) {
	d.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://tether"+path, rd)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

var (
	asSession      = map[string]string{"X-Test-Principal": "session", "X-Test-Scope": "catalog.write"}
	asBareSession  = map[string]string{"X-Test-Principal": "session"}
	asOperator     = map[string]string{"X-Test-Principal": "operator"}
	asAnonymousLoc = map[string]string{}
)

func (d *catalogApplyDaemon) plan(files []catalogapply.File, h map[string]string) catalogapply.Plan {
	d.t.Helper()
	code, b := d.call("POST", "/catalog/plans", api.CatalogPlanRequest{Name: "new-agent", Files: files}, h)
	if code != 200 {
		d.t.Fatalf("plan: %d %s", code, b)
	}
	var p catalogapply.Plan
	if err := json.Unmarshal(b, &p); err != nil {
		d.t.Fatal(err)
	}
	return p
}

var helperFiles = []catalogapply.File{{Path: "agents/helper.yaml", Content: "id: helper\nname: Helper\n"}}

// A worker session — the principal an agent holds — cannot approve its own plan,
// with the right digest and the right passphrase in hand.
func TestSessionCannotApproveItsOwnPlan(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	p := d.plan(helperFiles, asSession)
	body := api.CatalogApproveRequest{Digest: p.Digest, Passphrase: applyPass}

	for name, h := range map[string]map[string]string{
		"session with catalog.write": asSession,
		"session without scope":      asBareSession,
		"remote caller":              {"X-Test-Principal": "operator", "X-Test-Remote": "1"},
	} {
		code, b := d.call("POST", "/catalog/plans/"+p.ID+"/approve", body, h)
		if code != http.StatusForbidden {
			t.Errorf("%s: approve = %d %s, want 403", name, code, b)
		}
	}
	// And so it cannot apply.
	if code, _ := d.call("POST", "/catalog/plans/"+p.ID+"/apply", nil, asSession); code != http.StatusConflict {
		t.Errorf("apply of an unapproved plan = %d, want 409", code)
	}
	if _, err := os.Stat(filepath.Join(d.root, "agents/helper.yaml")); err == nil {
		t.Fatal("agent installed without approval")
	}
}

func TestOperatorApprovalNeedsThePassphrase(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	p := d.plan(helperFiles, asSession)

	if code, _ := d.call("POST", "/catalog/plans/"+p.ID+"/approve", api.CatalogApproveRequest{Digest: p.Digest, Passphrase: "guess guess guess"}, asOperator); code != http.StatusForbidden {
		t.Errorf("wrong passphrase = %d, want 403", code)
	}
	if code, _ := d.call("POST", "/catalog/plans/"+p.ID+"/approve", api.CatalogApproveRequest{Digest: "0000", Passphrase: applyPass}, asOperator); code != http.StatusConflict {
		t.Errorf("wrong digest = %d, want 409", code)
	}
	// The operator token alone, with no passphrase, is not enough.
	if code, _ := d.call("POST", "/catalog/plans/"+p.ID+"/approve", api.CatalogApproveRequest{Digest: p.Digest}, asOperator); code != http.StatusForbidden {
		t.Errorf("no passphrase = %d, want 403", code)
	}

	if code, b := d.call("POST", "/catalog/plans/"+p.ID+"/approve", api.CatalogApproveRequest{Digest: p.Digest, Passphrase: applyPass}, asOperator); code != 200 {
		t.Fatalf("approve = %d %s", code, b)
	}
	code, b := d.call("POST", "/catalog/plans/"+p.ID+"/apply", nil, asSession)
	if code != 200 {
		t.Fatalf("apply = %d %s", code, b)
	}
	var res catalogapply.Result
	_ = json.Unmarshal(b, &res)
	if !res.Applied || res.BackupID == "" || res.Activation != catalogapply.ActivationRestartRequired {
		t.Fatalf("result = %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Join(d.root, "agents/helper.yaml")); !strings.Contains(string(got), "id: helper") {
		t.Fatal("agent not installed")
	}
	if code, b := d.call("GET", "/catalog/backups", nil, nil); code != 200 || !strings.Contains(string(b), res.BackupID) {
		t.Fatalf("backups = %d %s", code, b)
	}
}

func TestSessionNeedsCatalogWriteToPlanOrApply(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	if code, _ := d.call("POST", "/catalog/plans", api.CatalogPlanRequest{Name: "x", Files: helperFiles}, asBareSession); code != http.StatusForbidden {
		t.Errorf("plan without catalog.write = %d, want 403", code)
	}
	if code, _ := d.call("POST", "/catalog/plans/cp_x/apply", nil, asBareSession); code != http.StatusForbidden {
		t.Errorf("apply without catalog.write = %d, want 403", code)
	}
	// Reading a plan needs no scope.
	p := d.plan(helperFiles, asSession)
	if code, _ := d.call("GET", "/catalog/plans/"+p.ID, nil, asBareSession); code != 200 {
		t.Errorf("get plan = %d", code)
	}
}

func TestSessionCannotChangeItsOwnDefinition(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	for _, path := range []string{"agents/manager.yaml", "launches/manager.yml"} {
		code, b := d.call("POST", "/catalog/plans", api.CatalogPlanRequest{Name: "self", Files: []catalogapply.File{{Path: path, Content: "id: manager\nnote: widen me\n"}}}, asSession)
		if code != http.StatusBadRequest || !strings.Contains(string(b), "own definition") {
			t.Errorf("%s: %d %s", path, code, b)
		}
	}
	// Another agent's definition is fine to plan.
	d.plan([]catalogapply.File{{Path: "agents/general.yaml", Content: "id: general\nname: Renamed\n"}}, asSession)
}

func TestRollbackGoesThroughTheSameGate(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	p := d.plan(helperFiles, asSession)
	d.call("POST", "/catalog/plans/"+p.ID+"/approve", api.CatalogApproveRequest{Digest: p.Digest, Passphrase: applyPass}, asOperator)
	_, b := d.call("POST", "/catalog/plans/"+p.ID+"/apply", nil, asSession)
	var res catalogapply.Result
	_ = json.Unmarshal(b, &res)

	code, b := d.call("POST", "/catalog/plans", api.CatalogPlanRequest{RollbackOf: res.BackupID}, asSession)
	if code != 200 {
		t.Fatalf("rollback plan = %d %s", code, b)
	}
	var rp catalogapply.Plan
	_ = json.Unmarshal(b, &rp)
	if rp.RollbackOf != res.BackupID {
		t.Fatalf("plan = %+v", rp)
	}
	if code, _ := d.call("POST", "/catalog/plans/"+rp.ID+"/apply", nil, asSession); code == 200 {
		t.Fatal("rollback applied without approval")
	}
	d.call("POST", "/catalog/plans/"+rp.ID+"/approve", api.CatalogApproveRequest{Digest: rp.Digest, Passphrase: applyPass}, asOperator)
	if code, b := d.call("POST", "/catalog/plans/"+rp.ID+"/apply", nil, asSession); code != 200 {
		t.Fatalf("rollback apply = %d %s", code, b)
	}
	if _, err := os.Stat(filepath.Join(d.root, "agents/helper.yaml")); err == nil {
		t.Fatal("rollback left the agent in place")
	}
}

func TestAnonymousLocalCallerCanPlanButNotApproveWithoutPassphrase(t *testing.T) {
	d := newCatalogApplyDaemon(t)
	p := d.plan(helperFiles, asAnonymousLoc)
	if code, _ := d.call("POST", "/catalog/plans/"+p.ID+"/approve", api.CatalogApproveRequest{Digest: p.Digest}, asAnonymousLoc); code != http.StatusForbidden {
		t.Errorf("approve with no passphrase = %d, want 403", code)
	}
}
