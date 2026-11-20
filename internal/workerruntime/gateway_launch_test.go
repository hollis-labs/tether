package workerruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/security/secrets/tokenfile"
	"github.com/hollis-labs/tether/internal/gatewaylaunch"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func TestWorkerGatewayMintsAndRefreshesBoundTokenOutsideWorkspace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux tmpfs")
	}
	device := filepath.Join(t.TempDir(), "device.token")
	deviceToken, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := tokenfile.Write(device, deviceToken); err != nil {
		t.Fatal(err)
	}
	var count atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/token" || r.Header.Get("Authorization") != "Bearer "+deviceToken {
			t.Error("wrong token exchange")
			w.WriteHeader(403)
			return
		}
		var request struct {
			Audience string   `json:"audience"`
			Session  string   `json:"session_id"`
			Scopes   []string `json:"scopes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Audience != "tether" || request.Session != "gateway-worker" || len(request.Scopes) != 1 || request.Scopes[0] != "tether.read" {
			t.Error("launcher grant changed")
		}
		n := count.Add(1)
		_ = json.NewEncoder(w).Encode(IssuedToken{Token: fmt.Sprintf("synthetic-lease-%d", n), ExpiresAt: time.Now().Add(500 * time.Millisecond), TTL: 1})
	}))
	defer issuer.Close()
	e := &Executor{Tokens: &TokenMinter{Hub: issuer.URL, DeviceTokenFile: device}}
	spec := workerwire.LaunchSpec{Session: "gateway-worker", Env: map[string]string{gatewaylaunch.URLenv: "https://mcp.example/mcp", "TETHER_RUN_TOKEN": "synthetic-run"}, AgentTokens: []workerwire.AgentTokenSpec{{Aud: "tether", Scopes: []string{"tether.read"}}}}
	path, stop, err := e.startGatewayCredential(spec)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if !strings.HasPrefix(path, "/dev/shm/") {
		t.Fatal("gateway credential in persistent workspace")
	}
	first, err := tokenfile.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	rotated := false
	for time.Now().Before(deadline) {
		current, err := tokenfile.Read(path)
		if err == nil && current != first {
			rotated = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !rotated {
		t.Fatal("worker gateway credential did not rotate")
	}
	stop()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("gateway lease retained at exit")
	}
	spec.AgentTokens = nil
	if _, _, err := e.startGatewayCredential(spec); err == nil {
		t.Fatal("gateway minted without launch grant")
	}
}

func TestWorkerGatewayEntryUsesProviderPlanters(t *testing.T) {
	for _, brand := range []string{"claude", "codex", "antigravity"} {
		t.Run(brand, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CODEX_HOME", t.TempDir())
			// Native Codex planting requires a captured source authentication stand-in.
			if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), []byte(`{"OPENAI_API_KEY":"synthetic"}`), 0600); err != nil {
				t.Fatal(err)
			}
			e, cred := httpPlantExecutor(t)
			root, cwd := t.TempDir(), t.TempDir()
			spec := workerwire.LaunchSpec{Session: "gateway", Provider: brand, RuntimeKind: "subprocess", MCPPolicy: true, Env: map[string]string{gatewaylaunch.URLenv: "https://mcp.example/mcp", gatewaylaunch.TokenFileEnv: "/dev/shm/tether-gateway-synthetic/gateway.token"}, AgentTokens: []workerwire.AgentTokenSpec{{Aud: "tether", Scopes: []string{"tether.read"}}}}
			if brand == "claude" {
				if err := e.plantHubMCP(cwd, cred, spec); err != nil {
					t.Fatal(err)
				}
				_, config := plantedTether(t, cwd)
				entry, ok := config.Servers["gateway"]
				if !ok || entry.Command != e.Binary || !strings.Contains(strings.Join(entry.Args, " "), "gateway.token") {
					t.Fatal("worker claude gateway not planted")
				}
				return
			}
			prepared, err := e.prepareProvider(context.Background(), spec, "/opt/"+brand, cwd, root)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			if err := filepath.WalkDir(prepared.PlantedBootDir, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				if d.Name() != "config.toml" && d.Name() != "mcp_config.json" {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if strings.Contains(string(data), "mcp-http-relay") && strings.Contains(string(data), "gateway.token") {
					found = true
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("worker gateway missing from native config")
			}
		})
	}
}
