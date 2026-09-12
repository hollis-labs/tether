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

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/mcpadapter"
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Start the MCP stdio adapter",
	Long: `Start an MCP stdio adapter that exposes the Tether runtime as tools.

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
  ai.invoke      — invoke mux_ai_chat over the local AI gateway
  catalog.write  — create and edit catalog agents (mux_agent_create / _edit)

Example MCP client config (mcp.json):
  {
    "mcpServers": {
      "tether": {
        "command": "mux",
        "args": ["mcp"],
        "env": {
          "AGENT_MUX_MCP_TOKEN": "your-token",
          "AGENT_MUX_MCP_SCOPES": "session.write,message.write,ai.invoke,catalog.write"
        }
      }
    }
  }`,
	RunE: runMCP,
}

var (
	mcpExtractRefs bool
	mcpSession     string
	mcpToken       string
	mcpScopes      string
	mcpProxy       bool
	mcpBroker      bool
	mcpServers     string
	mcpOnly        string
)

func init() {
	mcpCmd.Flags().BoolVar(&mcpExtractRefs, "extract-refs", false, "record identifiers seen in proxied tool arguments as session refs (requires --session; off by default)")
	mcpCmd.Flags().StringVar(&mcpSession, "session", "", "Tether session id this proxy serves; attributes proxied tool calls to it (set automatically in a launched worker's .mcp.json)")
	mcpCmd.Flags().StringVar(&mcpToken, "token", "", "auth token for mutating tools (env: AGENT_MUX_MCP_TOKEN)")
	mcpCmd.Flags().StringVar(&mcpScopes, "scopes", "", "comma-separated scopes: session.write,message.write,ai.invoke,catalog.write (env: AGENT_MUX_MCP_SCOPES)")
	mcpCmd.Flags().BoolVar(&mcpProxy, "proxy", false, "enable MCP proxy mode: load upstream servers from catalog/mcp-servers/ and merge their tools")
	mcpCmd.Flags().BoolVar(&mcpBroker, "broker", false, "enable broker mode (requires --proxy): register mux_discover+mux_call instead of all upstream tools; reduces per-request context size")
	mcpCmd.Flags().StringVar(&mcpServers, "servers", "", "comma-separated upstream server IDs to surface as native tools (env: MUX_MCP_SERVERS); empty = all servers when --proxy is set")
	mcpCmd.Flags().StringVar(&mcpOnly, "only", "", "curated proxy mode: expose only these comma-separated upstream server IDs as native tools; suppress Tether mux_* and discovery/call tools")
	_ = mcpCmd.Flags().MarkDeprecated("broker", "broker mode is superseded by --servers filtering; use --proxy with optional --servers instead")
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

	onlySet := cmd.Flags().Changed("only")
	serverFilter, curatedOnly, err := resolveMCPProxyConfig(mcpProxy, mcpBroker, mcpServers, mcpOnly, os.Getenv("MUX_MCP_SERVERS"), onlySet)
	if err != nil {
		return err
	}

	svc, err := app.New(expandCatalogPath())
	if err != nil {
		return fmt.Errorf("init service: %w", err)
	}
	defer func() { _ = svc.Close() }()

	// v005-09 rescue: route session-mutating MCP tools through the running
	// daemon over UDS to eliminate the in-process split-brain that the
	// pre-v005-09 code path produced (mux mcp's app.New() and muxd both
	// owned the same session store; daemon-only state like RuntimeManager
	// was invisible to mux mcp). Catalog reads, message ops, and read-only
	// session inspection still go in-process via svc — those are filesystem
	// or shared-DB reads that don't have the OS-process-state coupling.
	// When the daemon address can't be resolved the adapter falls back to
	// fully in-process behavior so dev/test paths still work.
	listenAddr, _ := resolveDaemonAddr()
	var adapter *mcpadapter.Adapter
	if listenAddr != "" {
		adapter = mcpadapter.NewWithDaemon(svc, client.New(listenAddr), token, scopes)
	} else {
		adapter = mcpadapter.New(svc, token, scopes)
	}
	adapter.SessionID = mcpSession
	if mcpExtractRefs {
		if listenAddr == "" {
			return fmt.Errorf("--extract-refs needs the daemon: refs are written over HTTP because `mux mcp` runs in its own process, and the daemon address could not be resolved")
		}
		if mcpSession == "" {
			return fmt.Errorf("--extract-refs needs --session: an extracted ref attaches to a session, and there is nothing to attach to without one")
		}
		adapter.ExtractRefs = true
		adapter.SetRefAttacher(refAttacherClient{c: client.New(listenAddr)})
	}
	// Route the go-mcp-sanitize middleware's warn telemetry to stderr so the
	// stdio MCP protocol stream on stdout stays clean.
	adapter.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	if mcpProxy {
		// Wire observability — LoggingMiddleware + in-memory ToolCallEventStore
		// (consumed by anyone subscribing to the event bus) + durable proxy_events
		// table (queryable via the mux_events_tool_calls MCP tool).
		eventStore := mcpadapter.NewToolCallEventStore(1000)
		opts := mcpadapter.ProxyOptions{
			Bus:          svc.Bus,
			EventStore:   eventStore,
			ProxyStore:   svc.Store, // durable SQLite store for mux_events_tool_calls
			BrokerMode:   mcpBroker, // deprecated path, still works
			ServerFilter: serverFilter,
			Only:         curatedOnly,
		}

		// Forward tool_call_end events to the running muxd daemon's event bus so
		// any consumer (HTTP /events SSE, MCP tools, downstream subscribers) can
		// observe proxy activity without sharing in-process memory. Best-effort:
		// if the daemon is not reachable the MCP adapter still works normally —
		// the events just don't reach the daemon's bus.
		//
		// Subscribe live-only: get the current max event seq so the forwarder
		// skips history replay. Replaying history causes a flood of stale
		// events on every startup and blocks live event delivery during drain.
		sinceSeq, seqErr := svc.Store.MaxEventSeq()
		if seqErr != nil {
			slog.Warn("mcp: could not read max event seq; forwarding from seq 0", "err", seqErr)
			sinceSeq = 0
		}
		daemonListenAddr, daemonBaseURL := resolveDaemonAddr()
		if daemonBaseURL != "" {
			go forwardProxyEventsToDaemon(cmd.Context(), svc.Bus, daemonListenAddr, daemonBaseURL, sinceSeq)
		}

		return adapter.RunWithProxyOpts(cmd.Context(), expandCatalogPath(), opts)
	}
	return adapter.Run(cmd.Context())
}

// resolveDaemonAddr derives the daemon's listen address and HTTP base URL from
// the catalog global config. Returns empty strings when the config cannot be
// loaded. listenAddr is needed to construct a socket-aware HTTP client for unix
// transport; baseURL is the http:// prefix used in request URLs.
func resolveDaemonAddr() (listenAddr, baseURL string) {
	cfg, err := loadDaemonConfig(catalogPath)
	if err != nil {
		slog.Debug("mcp: cannot resolve daemon address for event forwarding", "err", err)
		return "", ""
	}
	// cfg.ListenAddr is already tilde-expanded by daemonConfigFromCatalog →
	// expandListenAddr. Do NOT call config.Expand again — it calls filepath.Abs
	// which corrupts "unix:/path" into "<cwd>/unix:/path".
	addr := cfg.ListenAddr
	return addr, daemon.BaseURL(addr)
}

func resolveMCPProxyConfig(proxy, broker bool, serversFlag, onlyFlag, serversEnv string, onlySet bool) ([]string, bool, error) {
	if broker && !proxy {
		return nil, false, fmt.Errorf("--broker requires --proxy")
	}
	if onlySet && !proxy {
		return nil, false, fmt.Errorf("--only requires --proxy")
	}
	if onlySet && broker {
		return nil, false, fmt.Errorf("--only cannot be combined with --broker")
	}
	if onlySet && strings.TrimSpace(serversFlag) != "" {
		return nil, false, fmt.Errorf("--only cannot be combined with --servers")
	}
	if !proxy {
		return nil, false, nil
	}

	serversStr := serversFlag
	if onlySet {
		serversStr = onlyFlag
	}
	if !onlySet && serversStr == "" {
		serversStr = serversEnv
	}
	serverFilter := splitCommaList(serversStr)
	if onlySet && len(serverFilter) == 0 {
		return nil, false, fmt.Errorf("--only requires a non-empty comma-separated server list")
	}
	return serverFilter, onlySet, nil
}

func splitCommaList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
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
// listenAddr is used to construct a transport capable of dialing unix sockets.
// sinceSeq should be set to the current max event seq so only live events
// are forwarded — passing 0 causes full history replay on every startup.
func forwardProxyEventsToDaemon(ctx context.Context, bus events.Bus, listenAddr, daemonBaseURL string, sinceSeq int64) {
	ch, cancel, err := bus.Subscribe(ctx, events.Filter{SinceSeq: sinceSeq})
	if err != nil {
		slog.Warn("mcp: event forwarder failed to subscribe", "err", err)
		return
	}
	defer cancel()

	// Use daemon.DialHTTPClient so the transport can dial unix:// sockets.
	// A plain http.Client cannot reach "http://unix/..." addresses.
	httpClient := daemon.DialHTTPClient(listenAddr)
	httpClient.Timeout = 3 * time.Second
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

// refAttacherClient adapts the daemon client to the narrow seam the mcp
// adapter needs. The adapter cannot depend on api's request types directly --
// it is the proxy, not the daemon -- so the shape is flattened here.
type refAttacherClient struct{ c *client.Client }

func (r refAttacherClient) AttachSessionRef(ctx context.Context, sessionID, kind, refID, relation, source string) error {
	_, err := r.c.AttachSessionRef(ctx, sessionID, api.SessionRefAttachRequest{
		Kind: kind, RefID: refID, Relation: relation, Source: source,
	})
	return err
}
