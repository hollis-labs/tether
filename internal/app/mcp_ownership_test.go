package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
)

func TestDaemonOwnershipRejectsUnusableLaunch(t *testing.T) {
	for _, mode := range []string{"invalid", "daemon", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			svc, rt, id, _ := credentialLaunch(t)
			svc.Catalog.Global.Daemon.MCPUpstreams = mode
			if mode == "disabled" {
				svc.Catalog.Global.Daemon.MCPUpstreams = config.MCPUpstreamsDaemon
			}
			svc.Catalog.Global.Daemon.ListenAddr = "unix:" + t.TempDir() + "/missing.sock"
			if mode == "disabled" {
				listener, err := net.Listen("unix", strings.TrimPrefix(svc.Catalog.Global.Daemon.ListenAddr, "unix:"))
				if err != nil {
					t.Fatal(err)
				}
				server := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}
				go func() { _ = server.Serve(listener) }()
				defer func() { _ = server.Close() }()
			}
			_, err := svc.LaunchSessionWithContext(context.Background(), id)
			if err == nil {
				t.Fatal("unusable forwarding launch accepted")
			}
			if mode != "invalid" && !strings.Contains(err.Error(), "legacy_proxy") {
				t.Fatal("missing fix hint", err)
			}
			row, err := svc.Store.GetSession(id)
			if err != nil || row.State != "failed" {
				t.Fatalf("failed launch state: %+v %v", row, err)
			}
			if len(rt.options.Env) != 0 {
				t.Fatal("failed preflight reached agent runtime")
			}
			var active int
			if err := svc.Store.DB().QueryRow("SELECT count(*) FROM principals WHERE session_id=? AND revoked_at IS NULL", id).Scan(&active); err != nil || active != 0 {
				t.Fatalf("credential remained live: %d %v", active, err)
			}
		})
	}
}

func TestDaemonWorkerEnvExcludesUpstreamCredentials(t *testing.T) {
	svc, _, _, _ := credentialLaunch(t)
	if err := os.MkdirAll(filepath.Join(svc.CatalogRoot, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	body := "id: app\ntransport: stdio\ncommand: /must-not-spawn\nenv:\n  APP_SECRET: ${APP_SOURCE_SECRET}\ntoken: ${REMOTE_SECRET}\nargs: [\"${ARG_SECRET}\"]\n"
	if err := os.WriteFile(filepath.Join(svc.CatalogRoot, "mcp-servers", "app.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := svc.daemonWorkerEnv([]string{"APP_SECRET=canary", "APP_SOURCE_SECRET=canary", "REMOTE_SECRET=canary", "ARG_SECRET=canary", "ANTHROPIC_API_KEY=model-auth", "PATH=/usr/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"ANTHROPIC_API_KEY=model-auth", "PATH=/usr/bin"}) {
		t.Fatalf("upstream credential in worker env: %v", got)
	}
}

func TestDaemonOwnershipPlantsConsumedProviderConfig(t *testing.T) {
	for _, brand := range []string{"claude", "opencode", "codex"} {
		t.Run(brand, func(t *testing.T) {
			svc, rt, id, ws := credentialLaunch(t, brand)
			if brand == "opencode" {
				plan, err := svc.Store.GetLaunchPlan(id)
				if err != nil {
					t.Fatal(err)
				}
				plan.RuntimeKind = config.RuntimeKindSubprocess
				raw, _ := json.Marshal(plan)
				if _, err := svc.Store.DB().Exec("UPDATE launch_plans SET plan_json=? WHERE session_id=?", string(raw), id); err != nil {
					t.Fatal(err)
				}
			}
			svc.Catalog.Global.Daemon.MCPUpstreams = config.MCPUpstreamsDaemon
			ids := identity.NewStore(svc.Store.DB())
			stub := mcp.NewServer(&mcp.Implementation{Name: "daemon-preflight", Version: "1"}, nil)
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return stub }, nil)
			root, err := os.MkdirTemp("/var/tmp", "plant-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			addr := "unix:" + filepath.Join(root, "daemon.sock")
			listener, err := net.Listen("unix", strings.TrimPrefix(addr, "unix:"))
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				p, err := ids.Verify(r.Context(), token)
				if err != nil || p.SessionID != id {
					http.Error(w, "invalid session", http.StatusUnauthorized)
					return
				}
				handler.ServeHTTP(w, r)
			}), ReadHeaderTimeout: time.Second}
			go func() { _ = server.Serve(listener) }()
			defer server.Close()
			svc.Catalog.Global.Daemon.ListenAddr = addr
			launched, err := svc.LaunchSession(id)
			if err != nil {
				t.Fatal(err)
			}
			var token string
			for _, env := range rt.options.Env {
				if strings.HasPrefix(env, "TETHER_TOKEN=") {
					token = strings.TrimPrefix(env, "TETHER_TOKEN=")
				}
			}
			if token == "" {
				t.Fatal("missing session token")
			}
			target := ".mcp.json"
			if brand == "opencode" {
				target = "opencode.json"
			}
			if brand == "codex" {
				target = "config.toml"
			}
			found := false
			err = filepath.WalkDir(filepath.Join(ws, "boot"), func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() || d.Name() != target {
					return nil
				}
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				text := string(body)
				if !strings.Contains(text, "--forward-daemon") || !strings.Contains(text, addr) || !strings.Contains(text, token) {
					return fmt.Errorf("consumed %s config missing forwarding route", brand)
				}
				if strings.Contains(text, "--proxy") || strings.Contains(text, "--catalog") || strings.Contains(text, "--token\"") {
					return fmt.Errorf("legacy/upstream flags in %s config", brand)
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				if info.Mode().Perm() != 0600 {
					return fmt.Errorf("credential config permissions: %o", info.Mode().Perm())
				}
				found = true
				return nil
			})
			if err != nil || !found {
				t.Fatalf("consumed provider planting: %v (found=%v)", err, found)
			}
			if strings.Contains(strings.Join(launched.Plan.Args, " "), token) {
				t.Fatal("token in launch argv")
			}
			policy, err := svc.Store.SessionMCPPolicy(context.Background(), id)
			if err != nil || policy.UpstreamOwnership != config.MCPUpstreamsDaemon {
				t.Fatalf("ownership snapshot: %+v %v", policy, err)
			}
		})
	}
}
