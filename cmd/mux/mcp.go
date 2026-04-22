package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/app"
	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/daemon"
	"github.com/chrispian/agent-mux/internal/events"
	"github.com/chrispian/agent-mux/internal/mcpadapter"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start the MCP stdio adapter",
	Long: `Start an MCP stdio adapter that exposes the agent-mux runtime as tools.

The adapter communicates over stdin/stdout using the MCP protocol. Configure
your MCP client (Claude Desktop, Cursor, etc.) to run this command.

Mutating tools (session.create/launch/stop, message.send, etc.) require a
token and the corresponding scope. Pass them via flags or environment
variables:

  AGENT_MUX_MCP_TOKEN=<token> AGENT_MUX_MCP_SCOPES=session.write,message.write \
    mux mcp

Available scopes:
  session.write  — create, launch, stop, wait, input, resize, resume sessions
  message.write  — send, consume, cancel messages

Example MCP client config (mcp.json):
  {
    "mcpServers": {
      "agent-mux": {
        "command": "mux",
        "args": ["mcp"],
        "env": {
          "AGENT_MUX_MCP_TOKEN": "your-token",
          "AGENT_MUX_MCP_SCOPES": "session.write,message.write"
        }
      }
    }
  }`,
	RunE: runMCP,
}

var (
	mcpToken  string
	mcpScopes string
	mcpProxy  bool
	mcpBroker bool
)

func init() {
	mcpCmd.Flags().StringVar(&mcpToken, "token", "", "auth token for mutating tools (env: AGENT_MUX_MCP_TOKEN)")
	mcpCmd.Flags().StringVar(&mcpScopes, "scopes", "", "comma-separated scopes: session.write,message.write (env: AGENT_MUX_MCP_SCOPES)")
	mcpCmd.Flags().BoolVar(&mcpProxy, "proxy", false, "enable MCP proxy mode: load upstream servers from catalog/mcp-servers/ and merge their tools")
	mcpCmd.Flags().BoolVar(&mcpBroker, "broker", false, "enable broker mode (requires --proxy): register mux_discover+mux_call instead of all upstream tools; reduces per-request context size")
}

func runMCP(cmd *cobra.Command, _ []string) error {
	// Flags take precedence; fall back to env vars.
	token := mcpToken
	if token == "" {
		token = os.Getenv("AGENT_MUX_MCP_TOKEN")
	}
	scopeStr := mcpScopes
	if scopeStr == "" {
		scopeStr = os.Getenv("AGENT_MUX_MCP_SCOPES")
	}
	scopes := splitScopes(scopeStr)

	svc, err := app.New(expandCatalogPath())
	if err != nil {
		return fmt.Errorf("init service: %w", err)
	}
	defer func() { _ = svc.Close() }()

	adapter := mcpadapter.New(svc, token, scopes)
	if mcpProxy {
		// Wire observability — LoggingMiddleware + ToolCallEventStore.
		store := mcpadapter.NewToolCallEventStore(1000)
		opts := mcpadapter.ProxyOptions{
			Bus:        svc.Bus,
			EventStore: store,
			BrokerMode: mcpBroker,
		}

		// Forward tool_call_end events to the running muxd daemon so the TUI
		// Activity feed can display them without sharing in-process memory.
		// Best-effort: if the daemon is not reachable the MCP adapter still
		// works normally — we just don't get TUI visibility.
		daemonBaseURL := resolveDaemonBaseURL()
		if daemonBaseURL != "" {
			go forwardProxyEventsToDaemon(cmd.Context(), svc.Bus, daemonBaseURL)
		}

		return adapter.RunWithProxyOpts(cmd.Context(), expandCatalogPath(), opts)
	}
	return adapter.Run(cmd.Context())
}

// resolveDaemonBaseURL derives the daemon's HTTP base URL from the catalog
// global config. Returns an empty string when the config cannot be loaded or
// the daemon address is unknown.
func resolveDaemonBaseURL() string {
	cfg, err := loadDaemonConfig(catalogPath)
	if err != nil {
		slog.Debug("mcp: cannot resolve daemon address for event forwarding", "err", err)
		return ""
	}
	return daemon.BaseURL(config.Expand(cfg.ListenAddr))
}

// proxyEventForwardBody is the POST /proxy/events request shape.
type proxyEventForwardBody struct {
	SessionID    string `json:"session_id,omitempty"`
	Server       string `json:"server"`
	ToolName     string `json:"tool_name"`
	ArgsSchemaFP string `json:"args_schema_fp,omitempty"`
	DurationMs   int64  `json:"duration_ms"`
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	Timestamp    string `json:"timestamp"`
}

// forwardProxyEventsToDaemon subscribes to the bus and POSTs every
// tool_call_end event to daemonBaseURL/proxy/events. Runs until ctx is done.
func forwardProxyEventsToDaemon(ctx context.Context, bus events.Bus, daemonBaseURL string) {
	ch, cancel, err := bus.Subscribe(ctx, events.Filter{})
	if err != nil {
		slog.Warn("mcp: event forwarder failed to subscribe", "err", err)
		return
	}
	defer cancel()

	httpClient := &http.Client{Timeout: 3 * time.Second}
	url := daemonBaseURL + "/proxy/events"

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Kind != events.EventTypeToolCallEnd {
				continue
			}

			// Unmarshal the ToolCallEvent payload.
			var tce events.ToolCallEvent
			if err := json.Unmarshal([]byte(ev.PayloadJSON), &tce); err != nil {
				slog.Warn("mcp: forwarder failed to unmarshal ToolCallEvent", "err", err)
				continue
			}

			body := proxyEventForwardBody{
				SessionID:    tce.SessionID,
				Server:       tce.Server,
				ToolName:     tce.ToolName,
				ArgsSchemaFP: tce.ArgsSchemaFP,
				DurationMs:   tce.DurationMs,
				OK:           tce.OK,
				Error:        tce.Error,
				Timestamp:    tce.Timestamp.UTC().Format(time.RFC3339Nano),
			}
			raw, _ := json.Marshal(body)
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
			if reqErr != nil {
				slog.Warn("mcp: forwarder build request error", "err", reqErr)
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			resp, doErr := httpClient.Do(req)
			if doErr != nil {
				// Daemon not running — log at debug and continue; don't spam.
				slog.Debug("mcp: forwarder POST failed (daemon unreachable?)", "err", doErr)
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				slog.Warn("mcp: forwarder POST unexpected status", "status", resp.StatusCode)
			}
		}
	}
}

func splitScopes(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
