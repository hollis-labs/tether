package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamstore"
	"github.com/hollis-labs/tether/internal/teamsvc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const daemonTeamCaller mesh.URN = "msg://service/local/member"

func teamRequire(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type daemonTeamFixture struct {
	svc    *app.Service
	cfg    daemon.Config
	ops    *teamsvc.Service
	ids    *identity.Store
	server *daemon.Server
}

func newDaemonTeamFixture(t *testing.T, enabled *bool) *daemonTeamFixture {
	t.Helper()
	root, err := os.MkdirTemp("/var/tmp", "team-daemon-")
	teamRequire(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	catalog := filepath.Join(root, "catalog")
	teamRequire(t, os.Mkdir(catalog, 0700))
	flag := ""
	if enabled != nil {
		flag = fmt.Sprintf("teams:\n  enabled: %t\n", *enabled)
	}
	raw := fmt.Sprintf("%scatalog:\n  defaults:\n    state_db: %q\ndaemon:\n  listen_addr: %q\n  pid_file: %q\n  shutdown_timeout: 2s\n  mcp_endpoint:\n    enabled: true\nidentity:\n  mode: enforce\nmcp:\n  discovery_mode: flat\n", flag, filepath.Join(root, "state.db"), "unix:"+filepath.Join(root, "d.sock"), filepath.Join(root, "d.pid"))
	teamRequire(t, os.WriteFile(filepath.Join(catalog, "global.yaml"), []byte(raw), 0600))
	cat, err := config.Load(catalog)
	teamRequire(t, err)
	cfg, err := daemonConfigFromCatalog(cat)
	teamRequire(t, err)
	db, err := store.Open(filepath.Join(root, "state.db"))
	teamRequire(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc := &app.Service{Store: db, Catalog: cat, CatalogRoot: catalog, Registry: registry.NewService(registry.NewStorage(db.DB()))}
	ops, closeTeams, err := buildDaemonTeams(svc, cfg)
	teamRequire(t, err)
	t.Cleanup(closeTeams)
	f := &daemonTeamFixture{svc: svc, cfg: cfg, ops: ops, ids: identity.NewStore(db.DB())}
	f.server = &daemon.Server{Config: cfg, Identity: f.ids, Teams: ops, Service: &ordinaryTeamLaunch{}, MessageStore: db.MessagingStore()}
	t.Cleanup(f.server.CloseIdentityAudit)
	return f
}

// No subprocesses or real model runtime are used. The Unix fixture exercises
// Server.Run's actual ConnContext and mux; cancellation joins the server before
// the MCP handler/content/store cleanup hooks run.
func (f *daemonTeamFixture) serve(t *testing.T, tcp bool) {
	t.Helper()
	var live *httptest.Server
	if tcp {
		live = httptest.NewUnstartedServer(nil)
		f.cfg.ListenAddr = "tcp:" + live.Listener.Addr().String()
		f.server.Config = f.cfg
	}
	h, err := buildDaemonMCP(context.Background(), f.svc, f.cfg, f.ids, f.ops)
	teamRequire(t, err)
	f.server.MCP = h
	f.server.MCPShutdown = h.Close
	f.server.MCPDrain = h.Drain
	t.Cleanup(h.Close)
	if tcp {
		live.Config.Handler = f.server.Handler()
		live.Config.ConnContext = identity.ConnectionContext
		live.Start()
		t.Cleanup(live.Close)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.server.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("fixture daemon did not drain")
		}
	})
	c := f.httpClient(t)
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, err := http.NewRequest(http.MethodGet, "http://unix/health", nil)
		teamRequire(t, err)
		res, err := c.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == 200 {
				return
			}
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("daemon failed before serving: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture daemon did not serve")
		}
		time.Sleep(time.Millisecond * 5)
	}
}

func (f *daemonTeamFixture) httpClient(t *testing.T) *http.Client {
	t.Helper()
	transport := &http.Transport{}
	if strings.HasPrefix(f.cfg.ListenAddr, "unix:") {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(f.cfg.ListenAddr, "unix:"))
		}
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}
func (f *daemonTeamFixture) token(t *testing.T, id string, kind string, scopes ...string) string {
	t.Helper()
	token, err := f.ids.Mint(context.Background(), identity.Principal{ID: id, Kind: kind, Scopes: scopes})
	teamRequire(t, err)
	return token
}
func (f *daemonTeamFixture) post(t *testing.T, token, path, key string, value any, headers map[string]string) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(value)
	teamRequire(t, err)
	c := f.httpClient(t)
	url := "http://unix" + path
	if strings.HasPrefix(f.cfg.ListenAddr, "tcp:") {
		url = "http://" + strings.TrimPrefix(f.cfg.ListenAddr, "tcp:") + path
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	teamRequire(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	teamRequire(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	teamRequire(t, err)
	return res.StatusCode, body
}

type ordinaryTeamLaunch struct{ api.LaunchService }

func (*ordinaryTeamLaunch) LaunchSession(id string) (api.LaunchResult, error) {
	return api.LaunchResult{SessionID: id, ProviderID: "fixture", LogicalAgentID: "ordinary"}, nil
}

func daemonTeamDefinition() teams.Team {
	return teams.Team{ID: "fixture-team", Name: "Fixture", Version: 1,
		Slots:     []teams.Slot{{Name: "worker", Definition: mesh.DefinitionRef{ID: "worker", Revision: "r1", Digest: "verified-pin"}, Resolution: teams.Fresh, Activation: teams.Singleton, Min: 1, Max: 1}},
		Phases:    []teams.Phase{{ID: "work", Kind: "flex", ActiveSlots: []string{"worker"}, OwnerSlot: "worker", ExitTrigger: teams.Trigger{Kind: "event", Spec: map[string]string{"event": "done"}}}},
		Authority: teams.Authority{Mode: teams.Strict, Grants: []teams.Grant{{FromSlot: "worker", Verb: teams.MayMessage, ToSlot: "worker"}}},
		Policy:    teams.Policy{Spawn: teamsvc.ConservativePolicy().Limits}, Routing: teams.Routing{CoordinatorSlot: "worker"}}
}
func (f *daemonTeamFixture) seed(t *testing.T, actor mesh.URN, kind mesh.ActorKind) {
	t.Helper()
	s, err := teamstore.New(f.svc.Store.DB(), teamstore.Options{})
	teamRequire(t, err)
	teamRequire(t, s.PutDefinition(context.Background(), daemonTeamDefinition()))
	_, err = s.CreateRun(context.Background(), teams.TeamRun{ID: "retained", TeamID: "fixture-team", TeamVersion: 1, Status: mesh.TaskWorking})
	teamRequire(t, err)
	teamRequire(t, s.Mutate(context.Background(), "retained", func(r *teams.Roster) error {
		r.Members = []teams.Member{{ID: "member", Slot: "worker", Actor: actor, Kind: kind, Status: "active", Governance: teams.Owner, Resolution: teams.Durable}}
		return nil
	}))
}
func teamAddress() map[string]any {
	return map[string]any{"run_id": "retained", "address": "@worker", "body": "fixture notice", "kind": "service"}
}
func (f *daemonTeamFixture) deliveries(t *testing.T) int {
	t.Helper()
	var n int
	teamRequire(t, f.svc.Store.DB().QueryRow("SELECT count(*) FROM team_host_deliveries").Scan(&n))
	return n
}

func TestDaemonTeamsDefaultOffPreservesOrdinaryLaunchAndMessaging(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit_false=%t", explicit), func(t *testing.T) {
			var enabled *bool
			if explicit {
				off := false
				enabled = &off
			}
			f := newDaemonTeamFixture(t, enabled)
			if f.cfg.TeamsEnabled || api.HasTeamOps(f.ops) {
				t.Fatal("teams constructed by default")
			}
			// Even an accidentally supplied service must be masked while the flag is off.
			on := f.cfg
			on.TeamsEnabled = true
			ops, closeOps, err := buildDaemonTeams(f.svc, on)
			teamRequire(t, err)
			defer closeOps()
			f.ops = ops
			f.server.Teams = ops
			f.seed(t, daemonTeamCaller, mesh.ActorService)
			f.serve(t, false)
			token := f.token(t, string(daemonTeamCaller), "service", mcpadapter.ScopeTeamWrite)
			status, body := f.post(t, token, "/sessions/ordinary/launch", "", nil, nil)
			if status != 200 || !bytes.Contains(body, []byte(`"logical_agent_id":"ordinary"`)) {
				t.Fatalf("ordinary launch: %d %s", status, body)
			}
			status, body = f.post(t, token, "/messages", "", map[string]any{"from": daemonTeamCaller, "to": daemonTeamCaller, "kind": "notice", "payload": map[string]string{"body": "ordinary"}}, nil)
			if status != 201 {
				t.Fatalf("ordinary messaging: %d %s", status, body)
			}
			status, body = f.post(t, token, "/teams/address", "disabled", teamAddress(), nil)
			if status != 404 || f.deliveries(t) != 0 {
				t.Fatalf("disabled teams: %d %s", status, body)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			session, err := client.New(f.cfg.ListenAddr, client.WithToken(token)).ConnectMCP(ctx, client.MCPOptions{})
			teamRequire(t, err)
			defer session.Close()
			for tool, err := range session.Tools(ctx, nil) {
				teamRequire(t, err)
				if strings.HasPrefix(tool.Name, "tether_team_") {
					t.Fatal("disabled MCP tool visible", tool.Name)
				}
			}
		})
	}
}

func TestDaemonTeamsEnabledReachesSharedServiceAndRefusesAssertedPrincipals(t *testing.T) {
	on := true
	f := newDaemonTeamFixture(t, &on)
	f.seed(t, daemonTeamCaller, mesh.ActorService)
	f.serve(t, false)
	token := f.token(t, string(daemonTeamCaller), "service", mcpadapter.ScopeTeamWrite)
	for _, bearer := range []string{"", "invalid"} {
		status, body := f.post(t, bearer, "/teams/address?as="+string(daemonTeamCaller), "spoof", teamAddress(), map[string]string{"X-Principal": string(daemonTeamCaller), "X-Local-Operator": "true"})
		if status != 401 || f.deliveries(t) != 0 {
			t.Fatalf("asserted principal: %d %s", status, body)
		}
	}
	status, body := f.post(t, token, "/teams/address", "shared", teamAddress(), nil)
	if status != 200 || f.deliveries(t) != 1 {
		t.Fatalf("verified HTTP: %d %s", status, body)
	}
	var want teamsvc.Result
	teamRequire(t, json.Unmarshal(body, &want))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.New(f.cfg.ListenAddr, client.WithToken(token)).ConnectMCP(ctx, client.MCPOptions{})
	teamRequire(t, err)
	defer session.Close()
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_team_address", Arguments: map[string]any{"key": "shared", "request": teamAddress()}})
	if err != nil || result.IsError {
		t.Fatalf("verified native MCP: %+v %v", result, err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	teamRequire(t, err)
	var got teamsvc.Result
	teamRequire(t, json.Unmarshal(raw, &got))
	if fmt.Sprint(want.DeliveryKeys) != fmt.Sprint(got.DeliveryKeys) || len(got.DeliveryKeys) != 1 || f.deliveries(t) != 1 {
		t.Fatalf("HTTP/MCP did not share receipts: %s %+v", raw, want)
	}
}

func TestDaemonTeamsFormationRefusedBeforeDefinitionEnrollmentOrSessionEffects(t *testing.T) {
	// A disabled composition must not even dereference missing store/catalog.
	off, closeOff, err := buildDaemonTeams(nil, daemon.Config{})
	teamRequire(t, err)
	closeOff()
	if off != nil {
		t.Fatal("disabled host exists")
	}
	on := true
	f := newDaemonTeamFixture(t, &on)
	for _, table := range []string{"team_definitions", "team_runs", "team_port_intents", "registry_entries", "sessions"} {
		var n int
		teamRequire(t, f.svc.Store.DB().QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		if n != 0 {
			t.Fatal("construction had effects", table, n)
		}
	}
	def := daemonTeamDefinition()
	teamRequire(t, teams.Validate(def))
	principal := identity.Principal{ID: string(daemonTeamCaller), Kind: "service"}
	_, err = f.ops.Form(identity.WithPrincipal(context.Background(), principal), teamsvc.FormRequest{Key: "valid-form", Team: def})
	if !errors.Is(err, teamsvc.ErrDenied) {
		t.Fatalf("production formation not denied: %v", err)
	}
	for _, table := range []string{"team_definitions", "team_runs", "team_port_intents", "registry_entries", "runtime_bindings", "sessions"} {
		var n int
		teamRequire(t, f.svc.Store.DB().QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		if n != 0 {
			t.Fatal("formation had effects", table, n)
		}
	}
}

func teamResultHasCode(result *mcpsdk.CallToolResult, code string) bool {
	raw, _ := json.Marshal(result)
	return bytes.Contains(raw, []byte(code))
}

func TestDaemonNativeMCPPreservesVerifiedUnixOperatorProof(t *testing.T) {
	for _, tcp := range []bool{false, true} {
		t.Run(fmt.Sprintf("tcp=%t", tcp), func(t *testing.T) {
			on := true
			f := newDaemonTeamFixture(t, &on)
			f.seed(t, teamsvc.LocalOperator, mesh.ActorUser)
			f.serve(t, tcp)
			token := f.token(t, identity.OperatorID, "operator", mcpadapter.ScopeTeamWrite)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			session, err := client.New(f.cfg.ListenAddr, client.WithToken(token)).ConnectMCP(ctx, client.MCPOptions{})
			teamRequire(t, err)
			defer session.Close()
			input := teamAddress()
			input["kind"] = "user"
			result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_team_address", Arguments: map[string]any{"key": "operator", "request": input}, Meta: map[string]any{"local_operator": true, "principal": identity.OperatorID}})
			if err != nil {
				t.Fatal(err)
			}
			if tcp {
				if !result.IsError || !teamResultHasCode(result, "unauthenticated") || f.deliveries(t) != 0 {
					t.Fatalf("TCP granted operator: %+v", result)
				}
			} else if result.IsError || f.deliveries(t) != 1 {
				t.Fatalf("Unix proof lost at native boundary: %+v", result)
			}
		})
	}
}

func TestDaemonRunWiresTeamsOnlyWhenExplicitlyEnabled(t *testing.T) {
	oldFactory, oldRun, oldClose, oldCatalog := newDaemonService, runDaemonServer, closeDaemonService, catalogPath
	t.Cleanup(func() {
		newDaemonService, runDaemonServer, closeDaemonService, catalogPath = oldFactory, oldRun, oldClose, oldCatalog
	})
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f := newDaemonTeamFixture(t, &enabled)
			catalogPath = filepath.Join(t.TempDir(), "absent")
			newDaemonService = func(string) (*app.Service, error) { return f.svc, nil }
			called := false
			runDaemonServer = func(server *daemon.Server, _ context.Context) error {
				called = true
				if server.Config.TeamsEnabled != enabled || api.HasTeamOps(server.Teams) != enabled {
					t.Fatal("production daemon omitted/misgated team injection")
				}
				if server.MCP == nil || server.MCPShutdown == nil {
					t.Fatal("production MCP not composed")
				}
				defer server.MCPShutdown()
				status := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/teams/form", strings.NewReader(`{}`))
				token := f.token(t, string(daemonTeamCaller), "service")
				req.Header.Set("Authorization", "Bearer "+token)
				server.Handler().ServeHTTP(status, req)
				want := 404
				if enabled {
					want = 400
				}
				if status.Code != want {
					t.Fatalf("production mount: %d %s", status.Code, status.Body.String())
				}
				server.CloseIdentityAudit()
				return nil
			}
			teamRequire(t, daemonRunCmd.RunE(daemonRunCmd, nil))
			if !called {
				t.Fatal("daemon never composed server")
			}
		})
	}
}

func TestDaemonTeamsObserveIdentityCannotAuthorizeTeamEffects(t *testing.T) {
	on := true
	f := newDaemonTeamFixture(t, &on)
	f.cfg.IdentityMode = identity.Observe
	f.server.Config = f.cfg
	ops, closeOps, err := buildDaemonTeams(f.svc, f.cfg)
	teamRequire(t, err)
	defer closeOps()
	f.ops = ops
	f.server.Teams = ops
	f.seed(t, daemonTeamCaller, mesh.ActorService)
	f.serve(t, false)
	token := f.token(t, string(daemonTeamCaller), "service", mcpadapter.ScopeTeamWrite)
	for _, bearer := range []string{"", "invalid", token} {
		status, body := f.post(t, bearer, "/teams/address", "observe", teamAddress(), map[string]string{"X-Principal": string(daemonTeamCaller)})
		if status != 401 || f.deliveries(t) != 0 {
			t.Fatalf("observe granted authority: %d %s", status, body)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.New(f.cfg.ListenAddr, client.WithToken(token)).ConnectMCP(ctx, client.MCPOptions{})
	teamRequire(t, err)
	defer session.Close()
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_team_address", Arguments: map[string]any{"key": "observe", "request": teamAddress()}})
	if err != nil || !result.IsError || f.deliveries(t) != 0 {
		t.Fatalf("observe MCP granted authority: %+v %v", result, err)
	}
}

func TestDaemonNativeMCPRejectsReuseWithoutAcceptedUnixProof(t *testing.T) {
	on := true
	f := newDaemonTeamFixture(t, &on)
	f.seed(t, teamsvc.LocalOperator, mesh.ActorUser)
	f.serve(t, false)
	token := f.token(t, identity.OperatorID, "operator", mcpadapter.ScopeTeamWrite)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.New(f.cfg.ListenAddr, client.WithToken(token)).ConnectMCP(ctx, client.MCPOptions{})
	teamRequire(t, err)
	defer session.Close()
	// Expose the same handler over a fixture TCP listener to exercise a reused
	// view. Use a Host accepted by both SDK policies so this refusal must come
	// from the transport guard against borrowing the original Unix proof.
	tcp := httptest.NewServer(f.server.Handler())
	defer tcp.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tcp.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	teamRequire(t, err)
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", session.ID())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := tcp.Client().Do(req)
	teamRequire(t, err)
	defer res.Body.Close()
	var refusal struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	teamRequire(t, json.NewDecoder(res.Body).Decode(&refusal))
	if res.StatusCode != http.StatusForbidden || refusal.Error.Code != "mcp_session_policy_mismatch" {
		t.Fatalf("view borrowed Unix proof: %d %+v", res.StatusCode, refusal)
	}
	if f.deliveries(t) != 0 {
		t.Fatal("cross-transport view caused team effects")
	}
}

func TestDaemonTeamsEnabledWithNilServiceExposesNoHTTPOrMCPVerbs(t *testing.T) {
	on := true
	f := newDaemonTeamFixture(t, &on)
	f.ops = nil
	f.server.Teams = f.ops // typed nil must not masquerade as availability
	f.serve(t, false)
	token := f.token(t, string(daemonTeamCaller), "service", mcpadapter.ScopeTeamWrite)
	status, body := f.post(t, token, "/teams/address", "absent", teamAddress(), nil)
	if status != 404 {
		t.Fatalf("typed-nil team service exposed: %d %s", status, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.New(f.cfg.ListenAddr, client.WithToken(token)).ConnectMCP(ctx, client.MCPOptions{})
	teamRequire(t, err)
	defer session.Close()
	for tool, err := range session.Tools(ctx, nil) {
		teamRequire(t, err)
		if strings.HasPrefix(tool.Name, "tether_team_") {
			t.Fatal("typed-nil MCP service exposed", tool.Name)
		}
	}
}
