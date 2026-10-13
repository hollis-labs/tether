package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	forumhost "github.com/hollis-labs/tether/internal/forums"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerwire"
)

type identityRig struct {
	t      *testing.T
	db     *store.Store
	ids    *identity.Store
	svc    *Service
	hub    *workerhub.Hub
	worker string
	specs  chan workerwire.LaunchSpec
	urn    string
	handle http.Handler
	op     string
}

// newIdentityRig wires a real hub, a real registry and the real daemon route table around a fake
// websocket worker that advertises features and records every launch spec it is sent.
func newIdentityRig(t *testing.T, features ...string) *identityRig {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ids := identity.NewStore(db.DB())
	hub := workerhub.New(db.DB())
	t.Cleanup(hub.Close)
	ctx := context.Background()
	expires := time.Now().Add(time.Hour)
	devToken, err := ids.Mint(ctx, identity.Principal{ID: "msg://device/fake-worker", Kind: "device", Scopes: []string{identity.ScopeWorker}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer((&daemon.Server{Identity: ids, Workers: hub, Config: daemon.Config{RemoteListener: daemon.RemoteListenerConfig{AllowedHosts: []string{"worker.test"}}}}).RemoteHandler("worker.test"))
	t.Cleanup(remote.Close)
	header := http.Header{}
	header.Set("Host", "worker.test")
	header.Set("Authorization", "Bearer "+devToken)
	header.Set("Tether-Protocol", "1")
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(remote.URL, "http")+workerwire.ConnectPath, header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	if err = ws.WriteJSON(workerwire.Frame{T: "hello", Proto: 1, Features: features, Worker: &workerwire.Worker{Label: "fake-worker", Platform: "linux", Arch: "amd64"}}); err != nil {
		t.Fatal(err)
	}
	var welcome workerwire.Frame
	if err = ws.ReadJSON(&welcome); err != nil || welcome.T != "welcome" {
		t.Fatalf("welcome: %+v %v", welcome, err)
	}
	specs := make(chan workerwire.LaunchSpec, 8)
	go func() {
		for {
			var f workerwire.Frame
			if ws.ReadJSON(&f) != nil {
				return
			}
			var spec workerwire.LaunchSpec
			_ = json.Unmarshal(f.Spec, &spec)
			specs <- spec
			_ = ws.WriteJSON(workerwire.Frame{T: "launched", CmdID: f.CmdID, Session: f.Session, PID: 7})
		}
	}()
	reg := registry.NewService(registry.NewStorage(db.DB()))
	profile, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Portfolio Manager"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: db, Workers: hub, Registry: reg, Catalog: &config.Catalog{}}
	hub.Event = svc.ingestWorkerFrame
	operator, err := ids.Mint(ctx, identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	server := &daemon.Server{Config: daemon.Config{IdentityMode: identity.Observe}, Identity: ids, Forums: forumhost.New(db), Service: identityTestService{svc: svc}, Registry: reg}
	workerID, err := hub.Resolve(ctx, "fake-worker")
	if err != nil {
		t.Fatal(err)
	}
	return &identityRig{t: t, db: db, ids: ids, svc: svc, hub: hub, worker: workerID, specs: specs, urn: profile.URN, handle: server.Handler(), op: operator}
}

func (r *identityRig) launch(id, urn string) (workerwire.LaunchSpec, error) {
	r.t.Helper()
	plan := &launch.Plan{LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderBrand: "claude", IdentityURN: urn}
	row := store.SessionRow{ID: id, WorkerID: r.worker, LaunchID: "pm", ProjectID: "tether", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderKind: "cli", Workspace: r.t.TempDir(), State: "created"}
	if err := r.db.CreateSession(row, plan); err != nil {
		r.t.Fatal(err)
	}
	_, err := r.svc.launchWorkerSession(context.Background(), &row, plan)
	if err != nil {
		return workerwire.LaunchSpec{}, err
	}
	select {
	case spec := <-r.specs:
		return spec, nil
	case <-time.After(3 * time.Second):
		r.t.Fatal("worker never received the launch")
		return workerwire.LaunchSpec{}, nil
	}
}

func (r *identityRig) call(method, path, body, credential string) (int, map[string]any) {
	r.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	w := httptest.NewRecorder()
	r.handle.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestWorkerSessionBearerResolvesToStableAgentURN(t *testing.T) {
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	var logs bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(orig) })

	spec, err := r.launch("sess-pm", r.urn)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Credential == "" || !slices.Equal(spec.MCPScopes, launchprofile.DefaultMCPScopes) {
		t.Fatalf("spec credential/scopes: %q %v", spec.Credential, spec.MCPScopes)
	}
	if strings.Contains(logs.String(), spec.Credential) {
		t.Fatal("credential leaked into hub logs")
	}
	for _, arg := range spec.Args {
		if strings.Contains(arg, spec.Credential) {
			t.Fatal("credential in launch args")
		}
	}
	for _, v := range spec.Env {
		if strings.Contains(v, spec.Credential) {
			t.Fatal("credential in launch env")
		}
	}

	code, ctxOut := r.call("GET", "/auth/context", "", spec.Credential)
	if code != 200 || ctxOut["principal_kind"] != "session" || ctxOut["session_id"] != "sess-pm" || ctxOut["agent_urn"] != r.urn {
		t.Fatalf("session /auth/context: %d %v", code, ctxOut)
	}
	// The operator path is unchanged: no session, no agent URN.
	code, opOut := r.call("GET", "/auth/context", "", r.op)
	if code != 200 || opOut["principal_kind"] != "operator" || opOut["session_id"] != nil && opOut["session_id"] != "" || opOut["agent_urn"] != nil && opOut["agent_urn"] != "" {
		t.Fatalf("operator /auth/context: %d %v", code, opOut)
	}

	// Forums: the operator grants the stable URN; the worker session posts as it.
	if code, out := r.call("POST", "/forums", `{"id":"portfolio","namespace":"app/torque/forum/portfolio","title":"Portfolio"}`, r.op); code != 201 {
		t.Fatalf("create forum: %d %v", code, out)
	}
	if code, _ := r.call("GET", "/forums/portfolio", "", spec.Credential); code != 403 {
		t.Fatalf("ungranted session read = %d, want 403", code)
	}
	if code, out := r.call("POST", "/forums/portfolio/participants", `{"principal_kind":"agent","principal_id":"`+r.urn+`","access":"post"}`, r.op); code != 200 {
		t.Fatalf("grant: %d %v", code, out)
	}
	code, list := r.call("GET", "/forums/portfolio/participants", "", r.op)
	if code != 200 || !strings.Contains(toJSON(list), r.urn) {
		t.Fatalf("participant_list: %d %v", code, list)
	}
	if code, _ := r.call("GET", "/forums/portfolio/participants", "", spec.Credential); code != 403 {
		t.Fatalf("agent participant_list = %d, want operator only", code)
	}
	code, topic := r.call("POST", "/forums/portfolio/topics", `{"title":"Hello"}`, spec.Credential)
	if code != 201 {
		t.Fatalf("agent topic: %d %v", code, topic)
	}
	topicID := topic["topic"].(map[string]any)["id"].(string)
	code, post := r.call("POST", "/forums/topics/"+topicID+"/posts", `{"body_md":"as PM"}`, spec.Credential)
	if code != 201 {
		t.Fatalf("agent post: %d %v", code, post)
	}
	stamp := post["post"].(map[string]any)["provenance"].(map[string]any)
	if stamp["id"] != r.urn || stamp["session_id"] != "sess-pm" || stamp["kind"] != "agent" {
		t.Fatalf("post provenance: %v", stamp)
	}

	// Session end revokes the bearer and releases the URN.
	exit := 0
	if err := r.svc.ingestWorkerFrame(r.worker, workerwire.Frame{T: "state", Session: "sess-pm", State: "exited", ExitCode: &exit}); err != nil {
		t.Fatal(err)
	}
	if code, _ := r.call("GET", "/auth/context", "", spec.Credential); code != 401 && code != 200 {
		t.Fatalf("revoked bearer status %d", code)
	} else if code == 200 {
		_, out := r.call("GET", "/auth/context", "", spec.Credential)
		if out["principal_kind"] == "session" {
			t.Fatalf("revoked bearer still resolves: %v", out)
		}
	}
	if _, err := r.ids.Verify(context.Background(), spec.Credential); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("Verify after end = %v", err)
	}
	if _, err := r.svc.Registry.CurrentBinding(context.Background(), r.urn); err == nil {
		t.Fatal("URN binding survived session end")
	}
}

func TestWorkerStableURNFailsClosedWhileLeased(t *testing.T) {
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	if _, err := r.launch("first", r.urn); err != nil {
		t.Fatal(err)
	}
	plan := &launch.Plan{LaunchID: "pm", LogicalAgentID: "portfolio-manager", ProviderID: "claude", IdentityURN: r.urn}
	row := store.SessionRow{ID: "second", WorkerID: r.worker, LaunchID: "pm", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderKind: "cli", Workspace: t.TempDir(), State: "created"}
	if err := r.db.CreateSession(row, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.launchWorkerSession(context.Background(), &row, plan); !errors.Is(err, ErrIdentityInUse) || !strings.Contains(err.Error(), "first") {
		t.Fatalf("second launch err = %v, want ErrIdentityInUse naming the holder", err)
	}
	if got, _ := r.db.GetSession("second"); got.State != "failed" {
		t.Fatalf("second session state = %s", got.State)
	}
	if b, err := r.svc.Registry.CurrentBinding(context.Background(), r.urn); err != nil || b.SessionID != "first" {
		t.Fatalf("holder lost the URN: %+v %v", b, err)
	}
	// Once the holder ended, a new session may take the URN.
	if _, err := r.svc.ForceStopSession(context.Background(), "first", api.ForceStopOptions{AllowLive: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.launch("third", r.urn); err != nil {
		t.Fatalf("relaunch after end: %v", err)
	}
}

func TestWorkerWithoutSessionCredFeatureKeepsLegacyPath(t *testing.T) {
	r := newIdentityRig(t, "bind", "deliver")
	spec, err := r.launch("legacy", "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Credential != "" || len(spec.MCPScopes) != 0 {
		t.Fatalf("legacy worker received credential: %+v", spec)
	}
	plan := &launch.Plan{LaunchID: "pm", LogicalAgentID: "portfolio-manager", ProviderID: "claude", IdentityURN: r.urn}
	row := store.SessionRow{ID: "needs-new-worker", WorkerID: r.worker, LaunchID: "pm", LogicalAgentID: "portfolio-manager", ProviderID: "claude", ProviderKind: "cli", Workspace: t.TempDir(), State: "created"}
	if err := r.db.CreateSession(row, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.launchWorkerSession(context.Background(), &row, plan); err == nil || !strings.Contains(err.Error(), "does not support session credentials") {
		t.Fatalf("URN agent on old worker err = %v", err)
	}
}

func TestApplyCatalogIdentityIgnoresTeamMembersAndOverrides(t *testing.T) {
	svc := &Service{Catalog: &config.Catalog{Agents: map[string]config.Agent{"pm": {ID: "pm", URN: "msg://agent/agent-mux/agt_x", MCPScopes: []string{"message.write"}}}}}
	plan := &launch.Plan{LogicalAgentID: "pm"}
	svc.applyCatalogIdentity(plan)
	if plan.IdentityURN != "msg://agent/agent-mux/agt_x" || !slices.Equal(plan.MCPScopes, []string{"message.write"}) {
		t.Fatalf("plan = %+v", plan)
	}
	member := &launch.Plan{LogicalAgentID: "pm#1", CatalogAgentID: "pm", TeamMember: true}
	svc.applyCatalogIdentity(member)
	if member.IdentityURN != "" {
		t.Fatal("team member took the base agent's stable URN")
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// identityTestService mirrors cmd/tether's serviceAdapter for the two methods the caller-context
// resolver consults; the compile-time assertion in cmd/tether/daemon.go keeps the real adapter honest.
type identityTestService struct {
	api.LaunchService
	svc *Service
}

func (s identityTestService) GetSession(id string) (*store.SessionRow, error) {
	return s.svc.GetSession(id)
}
func (s identityTestService) SessionIdentityURN(id string) string {
	return s.svc.SessionIdentityURN(id)
}
