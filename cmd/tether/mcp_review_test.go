package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/spf13/cobra"
)

func TestMCPModeDaemonSettings(t *testing.T) {
	t.Setenv("TETHER_MCP_DISCOVERY_MODE", "")
	os.Unsetenv("TETHER_MCP_DISCOVERY_MODE")
	for _, tc := range []struct {
		status             int
		body, mode, source string
		wantError          bool
	}{
		{200, `{"discovery_mode":"search"}`, "search", "tether_setting", false},
		{404, `old daemon`, "flat", "default", false},
		{500, `broken`, "", "", true},
		{200, `{"discovery_mode":"invalid"}`, "", "", true},
	} {
		t.Run(http.StatusText(tc.status)+tc.body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/settings/mcp" {
					t.Errorf("request=%s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.Flags().String("discovery-mode", "", "")
			in, err := resolveMCPModeInputs(cmd, &app.Service{Catalog: &config.Catalog{}}, client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://")))
			if tc.wantError {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := mcpgateway.ResolveMode(in)
			if err != nil || string(got.Mode) != tc.mode || got.Source != tc.source {
				t.Fatalf("selection=%+v %v", got, err)
			}
		})
	}
}

func TestBadGatewayModeDoesNotBrickCatalog(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte("mcp:\n  discovery_mode: typo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cat, err := config.Load(root)
	if err != nil {
		t.Fatalf("generic catalog load: %v", err)
	}
	if result := checkMCPDiscoveryMode(cat); result.Status != statusFail {
		t.Fatalf("doctor=%+v", result)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.Flags().String("discovery-mode", "", "")
	if _, err := resolveMCPModeInputs(cmd, &app.Service{Catalog: cat}, nil); err == nil {
		t.Fatal("gateway accepted typo")
	}
}

func TestMCPProfileSelectionAndModeTier(t *testing.T) {
	oldProfile, oldMode := mcpProfile, mcpDiscoveryMode
	t.Cleanup(func() { mcpProfile = oldProfile; mcpDiscoveryMode = oldMode })
	t.Setenv("TETHER_MCP_PROFILE", "reader")
	t.Setenv("TETHER_MCP_DISCOVERY_MODE", "")
	os.Unsetenv("TETHER_MCP_DISCOVERY_MODE")
	flat, search := "flat", "search"
	svc := &app.Service{Catalog: &config.Catalog{Global: config.Global{MCP: mcpgateway.Config{DiscoveryMode: &flat, Profiles: map[string]mcpgateway.Profile{"reader": {DiscoveryMode: &search}, "writer": {DiscoveryMode: &flat}}}}}}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.Flags().StringVar(&mcpProfile, "profile", "", "")
	cmd.Flags().StringVar(&mcpDiscoveryMode, "discovery-mode", "", "")
	selected, err := resolveMCPProfile(cmd, svc)
	if err != nil || selected.ID != "reader" || selected.Source != "environment" {
		t.Fatalf("profile=%+v %v", selected, err)
	}
	in, err := resolveMCPModeInputs(cmd, svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	mode, err := mcpgateway.ResolveMode(in)
	if err != nil || mode.Mode != mcpgateway.Search || mode.Source != "profile" {
		t.Fatalf("mode=%+v %v", mode, err)
	}
	if err := cmd.Flags().Set("profile", "writer"); err != nil {
		t.Fatal(err)
	}
	selected, err = resolveMCPProfile(cmd, svc)
	if err != nil || selected.ID != "writer" || selected.Source != "argument" {
		t.Fatalf("profile=%+v %v", selected, err)
	}
	t.Setenv("TETHER_MCP_PROFILE", "bogus")
	if _, err := resolveMCPProfile(cmd, svc); err == nil {
		t.Fatal("unknown env profile hidden by argument")
	}
	t.Setenv("TETHER_MCP_PROFILE", "reader")
	if err := cmd.Flags().Set("profile", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveMCPProfile(cmd, svc); err == nil {
		t.Fatal("empty explicit profile fell back")
	}
}
