package main

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/bootexec"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLICallerCredentialPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TETHER_TOKEN", "")
	t.Setenv("TETHER_MCP_TOKEN", "")
	oldSession := mcpSession
	mcpSession = ""
	t.Cleanup(func() { mcpSession = oldSession })
	oldCatalog, oldFile := catalogPath, tokenFilePath
	t.Cleanup(func() { catalogPath = oldCatalog; tokenFilePath = oldFile })
	catalogPath = filepath.Join(home, "custom", "catalog")
	tokenFilePath = ""
	defaultToken, _ := identity.NewToken()
	envToken, _ := identity.NewToken()
	fileToken, _ := identity.NewToken()
	defaultFile := filepath.Join(home, "custom", "run", "operator.token")
	explicit := filepath.Join(home, "explicit.token")
	if err := identity.WriteTokenFile(defaultFile, defaultToken); err != nil {
		t.Fatal(err)
	}
	if err := identity.WriteTokenFile(explicit, fileToken); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, env, file, want string }{
		{"default", "", "", defaultToken}, {"environment", envToken, "", envToken}, {"explicit", envToken, explicit, fileToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TETHER_TOKEN", tc.env)
			tokenFilePath = tc.file
			token, err := callerToken()
			if err != nil || token != tc.want {
				t.Fatal("wrong credential source", err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+tc.want {
					t.Error("CLI client selected wrong bearer")
				}
				_, _ = io.WriteString(w, `{"status":"ok"}`)
			}))
			defer server.Close()
			if err := daemonClient("tcp:" + strings.TrimPrefix(server.URL, "http://")).Ping(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
	tokenFilePath = explicit
	t.Setenv("TETHER_TOKEN", envToken)
	if err := os.Chmod(explicit, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := callerToken(); err == nil {
		t.Fatal("bad explicit file fell back to environment")
	}
	tokenFilePath = filepath.Join(home, "missing")
	if _, err := callerToken(); err == nil {
		t.Fatal("missing explicit file fell back")
	}
}

func TestSessionProxyNeverFallsBackToOperator(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TETHER_TOKEN", "")
	t.Setenv("TETHER_MCP_TOKEN", "")
	oldCatalog, oldFile, oldSession := catalogPath, tokenFilePath, mcpSession
	t.Cleanup(func() { catalogPath, tokenFilePath, mcpSession = oldCatalog, oldFile, oldSession })
	catalogPath, tokenFilePath = filepath.Join(home, "catalog"), ""
	operator, _ := identity.NewToken()
	file := filepath.Join(home, "run", "operator.token")
	if err := identity.WriteTokenFile(file, operator); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, session, marker string }{{"session flag", "id", ""}, {"planted marker", "", "tether-worker"}} {
		t.Run(tc.name, func(t *testing.T) {
			mcpSession = tc.session
			t.Setenv("TETHER_MCP_TOKEN", tc.marker)
			token, err := callerToken()
			if err != nil || token != "" {
				t.Fatal("proxy acquired operator credential", err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("proxy sent operator bearer")
				}
				_, _ = io.WriteString(w, `{"status":"ok"}`)
			}))
			defer server.Close()
			if err := daemonClient("tcp:" + strings.TrimPrefix(server.URL, "http://")).Ping(context.Background()); err != nil {
				t.Fatal(err)
			}
			sessionToken, _ := identity.NewToken()
			t.Setenv("TETHER_TOKEN", sessionToken)
			if token, err := callerToken(); err != nil || token != sessionToken {
				t.Fatal("proxy ignored its session environment", err)
			}
		})
	}
}

func TestBootProxyEnvironmentIsExplicitlyAnonymous(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	env := tetherEnvFromPlan(map[string]string{"TETHER_MCP_SERVERS": "clockwork", "TETHER_TOKEN": "operator-canary"})
	prepared, err := bootexec.PrepareClaudeTUI(&launch.Plan{ProviderBrand: "claude", RepoRoot: t.TempDir(), Command: "fake-claude"}, bootexec.Options{
		BootDirRoot: t.TempDir(), TetherCommand: "fake-tether", TetherArgs: []string{"mcp", "--proxy"}, TetherEnv: env, ParentEnv: []string{"PATH=/bin", "TETHER_TOKEN=operator-canary"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prepared.BootDir); _ = os.RemoveAll(prepared.WorkspaceDir) })
	body, err := os.ReadFile(filepath.Join(prepared.BootDir, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Servers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, server := range config.Servers {
		if server.Env["TETHER_MCP_TOKEN"] == "tether-worker" {
			found = true
			token, ok := server.Env["TETHER_TOKEN"]
			if !ok || token != "" {
				t.Fatal("boot proxy not explicitly anonymous")
			}
		}
	}
	if !found {
		t.Fatal("boot proxy lacks anonymous marker")
	}
}
