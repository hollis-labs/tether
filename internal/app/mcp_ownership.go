package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/store"
)

func (s *Service) mcpOwnership() (string, error) {
	if s.Catalog == nil {
		return config.MCPUpstreamsLegacy, nil
	}
	return config.ReadMCPUpstreamOwnership(s.CatalogRoot, s.Catalog.Global.Daemon)
}

// preflightDaemonMCP proves the session can initialize the strict endpoint.
// A health ping alone would miss disabled endpoints and unavailable identity.
func (s *Service) preflightDaemonMCP(ctx context.Context, token string) (string, error) {
	hint := "set daemon.mcp_endpoint.enabled=true with verified identity and retry, or select daemon.mcp_upstream_ownership=legacy_proxy"
	if s.Catalog == nil || token == "" {
		return "", fmt.Errorf("daemon MCP ownership requires a session credential; %s", hint)
	}
	addr := s.Catalog.Global.Daemon.ListenAddr
	if addr == "" {
		addr = "unix:~/.tether/run/tetherd.sock"
	}
	if !strings.HasPrefix(addr, "unix:") {
		return "", fmt.Errorf("daemon MCP ownership requires a unix: listener; %s", hint)
	}
	addr = "unix:" + config.Expand(strings.TrimPrefix(addr, "unix:"))
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	connection, err := client.New(addr, client.WithToken(token)).ConnectMCP(probe, client.MCPOptions{})
	if err != nil {
		// Never include an upstream or HTTP response which may echo a credential.
		return "", fmt.Errorf("daemon MCP endpoint disabled, unreachable or rejected the session; %s", hint)
	}
	_ = connection.Close()
	return addr, nil
}

func DaemonMCPPlant(addr, sessionID string) TetherMCPPlan {
	return TetherMCPPlan{
		Args:        []string{"mcp", "--forward-daemon", "--daemon-address", addr, "--session", sessionID},
		Attribution: store.RefAttributionNone,
	}
}

// daemonWorkerEnv keeps catalog-owned upstream credentials out of the entire
// agent tree, not merely the planted proxy's explicitly configured env. Provider
// authentication unrelated to an upstream entry remains intact.
func (s *Service) daemonWorkerEnv(env []string) ([]string, error) {
	entries, err := config.LoadMCPServerCatalog(s.CatalogRoot)
	if err != nil {
		return nil, fmt.Errorf("read upstream environment policy: fix MCP catalog entries")
	}
	excluded := map[string]bool{}
	for _, entry := range entries {
		for _, key := range entry.WorkerExcludedEnvironmentKeys() {
			excluded[key] = true
		}
	}
	clean := make([]string, 0, len(env))
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		if !excluded[key] {
			clean = append(clean, value)
		}
	}
	return clean, nil
}
