package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/app/proxyevents"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcpgateway"
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

  TETHER_TOKEN=<token> TETHER_MCP_SCOPES=session.write,message.write \
    tether mcp

Available scopes:
  session.write  — create, launch, stop, wait, input, resize, resume sessions
  message.write  — send, consume, cancel messages
  ai.invoke      — invoke tether_ai_chat over the local AI gateway
  catalog.write  — create and edit catalog agents (tether_agent_create / _edit)

Example MCP client config (mcp.json):
  {
    "mcpServers": {
      "tether": {
        "command": "tether",
        "args": ["mcp"],
        "env": {
          "TETHER_TOKEN": "tth_…",
          "TETHER_MCP_SCOPES": "session.write,message.write,ai.invoke,catalog.write"
        }
      }
    }
  }`,
	RunE: runMCP,
}

var (
	mcpExtractRefs   bool
	mcpSession       string
	mcpToken         string
	mcpScopes        string
	mcpProxy         bool
	mcpForwardDaemon bool
	mcpDaemonAddress string
	mcpDiscoveryMode string
	mcpProfiles      []string
	mcpServers       string
	mcpOnly          string
	mcpConfine       bool
	mcpDaemonOnly    bool
	mcpProtect       []string
)

func init() {
	mcpCmd.Flags().BoolVar(&mcpForwardDaemon, "forward-daemon", false, "relay admitted daemon MCP tools only; no local upstreams or fallback")
	mcpCmd.Flags().StringVar(&mcpDaemonAddress, "daemon-address", "", "explicit unix: daemon endpoint for --forward-daemon")
	mcpCmd.Flags().BoolVar(&mcpExtractRefs, "extract-refs", false, "record identifiers seen in proxied tool arguments as session refs (requires --session; off by default)")
	mcpCmd.Flags().StringVar(&mcpSession, "session", "", "Tether session id this proxy serves; attributes proxied tool calls to it (set automatically in a launched worker's .mcp.json)")
	mcpCmd.Flags().StringVar(&mcpToken, "token", "", "legacy adapter presence marker; daemon credentials use --token-file or TETHER_TOKEN")
	mcpCmd.Flags().StringVar(&mcpScopes, "scopes", "", "comma-separated scopes: session.write,message.write,ai.invoke,catalog.write (env: TETHER_MCP_SCOPES)")
	mcpCmd.Flags().StringVar(&mcpDiscoveryMode, "discovery-mode", "", "MCP discovery mode: flat (default) or search (env: TETHER_MCP_DISCOVERY_MODE)")
	mcpCmd.Flags().Bool("no-discover", false, "expose allowed tools directly (same as --discovery-mode flat)")
	mcpCmd.Flags().StringArrayVar(&mcpProfiles, "profile", nil, "gateway profile ID (env: TETHER_MCP_PROFILE)")
	mcpCmd.Flags().BoolVar(&mcpProxy, "proxy", false, "enable MCP proxy mode: load upstream servers from catalog/mcp-servers/ and merge their tools")
	mcpCmd.Flags().StringVar(&mcpServers, "servers", "", "comma-separated upstream server IDs permitted in every discovery mode (env: TETHER_MCP_SERVERS); omitted = all enabled upstreams")
	mcpCmd.Flags().StringVar(&mcpOnly, "only", "", "restrict to these upstream server IDs and omit native Tether targets; gateway status/discovery infrastructure follows the selected mode")
	mcpCmd.Flags().BoolVar(&mcpConfine, "confine", false, "confine the proxy to the --servers / TETHER_MCP_SERVERS list (requires --proxy): only those upstreams are loaded, started and reachable, tether_tool_call included; every --servers list now restricts loading and reachability; an omitted list with --confine selects none. Set automatically in a launched worker's .mcp.json")
	mcpCmd.Flags().BoolVar(&mcpDaemonOnly, "daemon-only", false, "never open the state database: read and write Tether state only through the running daemon, and refuse to start without one (set in a launched worker's .mcp.json)")
	mcpCmd.Flags().StringArrayVar(&mcpProtect, "protect-path", nil, "a directory this server must not write (repeatable): tether_agent_create and tether_agent_edit refuse a target under it with catalog_read_only, whether or not a sandbox also makes it read-only. Set automatically in a launched worker's .mcp.json, from the same decision that protects the agent's catalog, run and state directories")
}

func runMCP(cmd *cobra.Command, _ []string) error {
	if mcpForwardDaemon {
		return runMCPForwardDaemon(cmd)
	}
	// Verified daemon credential first; legacy adapter-only flags remain a
	// local fast check, never a substitute for a daemon bearer credential.
	token, err := callerToken()
	if err != nil {
		return err
	}
	if token == "" {
		token = mcpToken
	}
	if token == "" {
		token = os.Getenv("TETHER_MCP_TOKEN")
	}
	scopeStr := mcpScopes
	if scopeStr == "" {
		scopeStr = os.Getenv("TETHER_MCP_SCOPES")
	}
	scopes := splitScopes(scopeStr)

	onlySet := cmd.Flags().Changed("only")
	serverFilter, curatedOnly, err := resolveMCPProxyConfig(mcpProxy, mcpServers, mcpOnly, os.Getenv("TETHER_MCP_SERVERS"), onlySet)
	if err != nil {
		return err
	}
	// Explicit empty server lists restrict to none; they never fall back to env.
	if cmd.Flags().Changed("servers") && strings.TrimSpace(mcpServers) == "" {
		serverFilter = []string{}
	}
	if !cmd.Flags().Changed("servers") && !onlySet {
		if value, present := os.LookupEnv("TETHER_MCP_SERVERS"); present && strings.TrimSpace(value) == "" {
			serverFilter = []string{}
		}
	}
	if mcpConfine && !mcpProxy {
		return fmt.Errorf("--confine requires --proxy")
	}

	listenAddr, _ := resolveDaemonAddr()
	if mcpDaemonOnly {
		return runMCPDaemonOnly(cmd, listenAddr, token, scopes, serverFilter, curatedOnly)
	}

	svc, err := app.New(expandCatalogPath())
	if err != nil {
		return fmt.Errorf("init service: %w", err)
	}
	defer func() { _ = svc.Close() }()

	// v005-09 rescue: route session-mutating MCP tools through the running
	// daemon over UDS to eliminate the in-process split-brain that the
	// pre-v005-09 code path produced (tether mcp's app.New() and tetherd both
	// owned the same session store; daemon-only state like RuntimeManager
	// was invisible to tether mcp). Catalog reads, message ops, and read-only
	// session inspection still go in-process via svc — those are filesystem
	// or shared-DB reads that don't have the OS-process-state coupling.
	// When the daemon address can't be resolved the adapter falls back to
	// fully in-process behavior so dev/test paths still work.
	var adapter *mcpadapter.Adapter
	if listenAddr != "" {
		adapter = mcpadapter.NewWithDaemon(svc, daemonClient(listenAddr), token, scopes)
	} else {
		adapter = mcpadapter.New(svc, token, scopes)
	}
	if err := configureMCPAdapter(adapter, listenAddr); err != nil {
		return err
	}
	profile, err := resolveMCPProfile(cmd, svc)
	if err != nil {
		return err
	}
	modeInputs, err := resolveMCPModeInputs(cmd, svc, nil)
	if err != nil {
		return err
	}
	floors, err := resolveMCPToolFloors()
	if err != nil {
		return err
	}
	if mcpProxy {
		// Wire observability — LoggingMiddleware + in-memory ToolCallEventStore
		// (consumed by anyone subscribing to the event bus) + durable proxy_events
		// table (queryable via the tether_events_tool_calls MCP tool).
		eventStore := mcpadapter.NewToolCallEventStore(1000)
		opts := inProcessProxyOptions(svc, eventStore, serverFilter, curatedOnly, mcpConfine)
		opts.ModeInputs = modeInputs
		opts.Profile = profile
		opts.AuthorityProfiles = floors

		// Forward tool_call_end events to the running tetherd daemon's event bus so
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
			// Capture credential paths while this command still owns its flags.
			dc := daemonClient(daemonListenAddr)
			forwardCtx, stopForwarding := context.WithCancel(cmd.Context())
			forwardDone := make(chan struct{})
			go func() {
				defer close(forwardDone)
				forwardProxyEventsWithClient(forwardCtx, svc.Bus, dc, sinceSeq)
			}()
			defer func() {
				stopForwarding()
				// Forwarding is best-effort and must never hold up shutdown.
				timer := time.NewTimer(2 * time.Second)
				defer timer.Stop()
				select {
				case <-forwardDone:
				case <-timer.C:
				}
			}()
		}

		return runProxy(cmd.Context(), adapter, expandCatalogPath(), opts)
	}
	return adapter.RunWithGatewayOpts(cmd.Context(), expandCatalogPath(), mcpadapter.ProxyOptions{ModeInputs: modeInputs, Profile: profile, AuthorityProfiles: floors, Bus: svc.Bus}, false)
}

// configureMCPAdapter applies the flags every `tether mcp` mode shares.
func configureMCPAdapter(adapter *mcpadapter.Adapter, listenAddr string) error {
	adapter.SessionID = mcpSession
	if listenAddr != "" {
		adapter.SetCallerContextResolver(daemonClient(listenAddr).CallerContext)
	}
	// Wired here, where both the in-process and the daemon-only server pass,
	// so the refusal cannot be a control that quietly does nothing on one.
	adapter.SetProtectedPaths(mcpProtect)
	adapter.SetBuildMetadata(version, commit, buildDate)
	if mcpExtractRefs {
		if listenAddr == "" {
			return fmt.Errorf("--extract-refs needs the daemon: refs are written over HTTP because `tether mcp` runs in its own process, and the daemon address could not be resolved")
		}
		if mcpSession == "" {
			return fmt.Errorf("--extract-refs needs --session: an extracted ref attaches to a session, and there is nothing to attach to without one")
		}
		adapter.ExtractRefs = true
		adapter.SetRefAttacher(refAttacherClient{c: daemonClient(listenAddr)})
	}
	// Route go-mcp's sanitize middleware's warn telemetry to stderr so the
	// stdio MCP protocol stream on stdout stays clean.
	adapter.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	return nil
}

// errDaemonOnlyUnreachable is why a daemon-only `tether mcp` will not start.
const errDaemonOnlyUnreachable = "tether daemon unreachable; tether tools unavailable"

// runMCPDaemonOnly serves `tether mcp --daemon-only`, the server Tether plants in
// each agent (CW-20261001-0173). It never opens the state database: the
// Service holds only the catalog, every read and write of Tether's state goes
// to the daemon over its API, and a proxied tool call is recorded by the
// daemon (POST /proxy/events with publish) instead of in this process. So an
// agent's sandbox can keep the state directory read-only.
//
// Without a reachable daemon it fails closed: an agent launched while tetherd is
// down has no tether tools.
func runMCPDaemonOnly(cmd *cobra.Command, listenAddr, token string, scopes, serverFilter []string, curatedOnly bool) error {
	// A refusal to start is not a usage error: print the one line that says
	// why, not cobra's usage text after it.
	cmd.SilenceUsage = true
	if listenAddr == "" {
		return fmt.Errorf("%s: the daemon address could not be resolved from the catalog", errDaemonOnlyUnreachable)
	}
	dc := daemonClient(listenAddr)
	pingCtx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	err := dc.Ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("%s: %w", errDaemonOnlyUnreachable, err)
	}

	svc, err := app.NewCatalogOnly(expandCatalogPath())
	if err != nil {
		return fmt.Errorf("load catalog: %w", err)
	}
	adapter := mcpadapter.NewWithDaemon(svc, dc, token, scopes)
	if err := configureMCPAdapter(adapter, listenAddr); err != nil {
		return err
	}
	profile, err := resolveMCPProfile(cmd, svc)
	if err != nil {
		return err
	}
	modeInputs, err := resolveMCPModeInputs(cmd, svc, dc)
	if err != nil {
		return err
	}
	floors, err := resolveMCPToolFloors()
	if err != nil {
		return err
	}
	if !mcpProxy {
		return adapter.RunWithGatewayOpts(cmd.Context(), expandCatalogPath(), mcpadapter.ProxyOptions{ModeInputs: modeInputs, Profile: profile, AuthorityProfiles: floors, Publisher: mcpadapter.NewDaemonToolCallPublisher(cmd.Context(), dc)}, false)
	}
	opts := daemonOnlyProxyOptions(cmd.Context(), dc, serverFilter, curatedOnly, mcpConfine)
	opts.ModeInputs = modeInputs
	opts.Profile = profile
	opts.AuthorityProfiles = floors
	return runProxy(cmd.Context(), adapter, expandCatalogPath(), opts)
}

// runProxy serves the proxy with the options a path built. It is a variable so a
// test can capture those options without running a stdio server.
var runProxy = func(ctx context.Context, adapter *mcpadapter.Adapter, catalog string, opts mcpadapter.ProxyOptions) error {
	return adapter.RunWithProxyOpts(ctx, catalog, opts)
}

// proxyOptionsFor is what both ways of serving the proxy share: which upstreams
// it loads and exposes. A switch that decides what an agent can reach belongs
// here and not inline at either call site. The planted server of every launched
// agent takes the daemon-only path (CW-20261001-0173), the operator's own takes
// the in-process one, and a flag wired into only one of them is a control that
// quietly does nothing on the other: that is how --confine (CW-20261001-0227)
// was once accepted and ignored in daemon-only mode. The callers add the fields
// that depend on where state lives.
func proxyOptionsFor(serverFilter []string, curatedOnly, confine bool) mcpadapter.ProxyOptions {
	return mcpadapter.ProxyOptions{
		ServerFilter: serverFilter,
		Only:         curatedOnly,
		Confine:      confine,
	}
}

// inProcessProxyOptions is proxyOptionsFor plus the event wiring of a server that
// opens the state database itself.
func inProcessProxyOptions(svc *app.Service, eventStore *mcpadapter.ToolCallEventStore, serverFilter []string, curatedOnly, confine bool) mcpadapter.ProxyOptions {
	opts := proxyOptionsFor(serverFilter, curatedOnly, confine)
	opts.Bus = svc.Bus
	opts.EventStore = eventStore
	opts.ProxyStore = svc.Store // durable SQLite store for tether_events_tool_calls
	return opts
}

// daemonOnlyProxyOptions is proxyOptionsFor plus the event wiring of a server that
// never opens the database: the daemon records each call.
func daemonOnlyProxyOptions(ctx context.Context, dc *client.Client, serverFilter []string, curatedOnly, confine bool) mcpadapter.ProxyOptions {
	opts := proxyOptionsFor(serverFilter, curatedOnly, confine)
	opts.Publisher = mcpadapter.NewDaemonToolCallPublisher(ctx, dc)
	opts.ProxyStore = mcpadapter.DaemonProxyEvents{Client: dc}
	return opts
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

func resolveMCPProxyConfig(proxy bool, serversFlag, onlyFlag, serversEnv string, onlySet bool) ([]string, bool, error) {
	if onlySet && !proxy {
		return nil, false, fmt.Errorf("--only requires --proxy")
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

// forwardProxyEventsToDaemon subscribes to the bus and POSTs every
// tool_call_end event to daemonBaseURL/proxy/events. Runs until ctx is done.
// listenAddr is used to construct a transport capable of dialing unix sockets.
// sinceSeq should be set to the current max event seq so only live events
// are forwarded — passing 0 causes full history replay on every startup.
func forwardProxyEventsToDaemon(ctx context.Context, bus events.Bus, listenAddr, _ string, sinceSeq int64) {
	forwardProxyEventsWithClient(ctx, bus, daemonClient(listenAddr), sinceSeq)
}

func forwardProxyEventsWithClient(ctx context.Context, bus events.Bus, dc *client.Client, sinceSeq int64) {
	proxyevents.Forward(ctx, bus, func(ctx context.Context, req proxyevents.ProxyEventIngestRequest) error {
		return dc.IngestProxyEvent(ctx, api.ProxyEventIngestRequest(req))
	}, sinceSeq)
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

func (r refAttacherClient) AttachSessionRef(ctx context.Context, sessionID, kind, refID, uri, relation, source, parentItemID string) error {
	_, err := r.c.AttachSessionRef(ctx, sessionID, api.SessionRefAttachRequest{
		Kind: kind, RefID: refID, URI: uri, Relation: relation, Source: source, ParentItemID: parentItemID,
	})
	return err
}

// The stdio client's environment is a distinct precedence tier, not a daemon
// environment guessed by an HTTP caller. Resolution is immutable for this run.
func resolveMCPModeInputs(cmd *cobra.Command, svc *app.Service, dc *client.Client) (mcpgateway.ModeInputs, error) {
	profile, err := resolveMCPProfile(cmd, svc)
	if err != nil {
		return mcpgateway.ModeInputs{}, err
	}
	in := mcpgateway.ModeInputs{Gateway: svc.Catalog.Global.MCP.DiscoveryMode, Profile: profile.Profile}
	in.Explicit = explicitMCPModes(cmd)
	if value, present := os.LookupEnv("TETHER_MCP_DISCOVERY_MODE"); present {
		in.Environment = &value
	}
	if dc != nil {
		stored, err := dc.GetMCPSettings(cmd.Context())
		if err != nil {
			return in, fmt.Errorf("load daemon MCP setting: %w", err)
		}
		in.Setting = stored.DiscoveryMode
	} else if svc.Settings != nil {
		stored, err := svc.Settings.GetMCP(cmd.Context())
		if err != nil {
			return in, err
		}
		in.Setting = stored.DiscoveryMode
	}
	_, err = mcpgateway.ResolveMode(in)
	return in, err
}

func resolveMCPProfile(cmd *cobra.Command, svc *app.Service) (mcpgateway.ProfileSelection, error) {
	in := mcpgateway.ProfileInputs{}
	if cmd.Flags().Changed("profile") {
		values, err := cmd.Flags().GetStringArray("profile")
		if err != nil {
			return mcpgateway.ProfileSelection{}, err
		}
		for _, value := range values {
			in.Explicit = append(in.Explicit, mcpgateway.Selector{Value: value, Source: "argument"})
		}
	}
	if value, present := os.LookupEnv("TETHER_MCP_PROFILE"); present {
		in.Environment = &value
	}
	return mcpgateway.ResolveProfile(svc.Catalog.Global.MCP, in)
}

func explicitMCPModes(cmd *cobra.Command) []mcpgateway.Selector {
	var selectors []mcpgateway.Selector
	if cmd.Flags().Changed("discovery-mode") {
		selectors = append(selectors, mcpgateway.Selector{Value: mcpDiscoveryMode, Source: "argument"})
	}
	if value, _ := cmd.Flags().GetBool("no-discover"); value {
		selectors = append(selectors, mcpgateway.Selector{Value: "flat", Source: "no-discover"})
	}
	return selectors
}

func resolveMCPToolFloors() ([]mcpgateway.ProfileSelection, error) {
	value, set := os.LookupEnv(mcpgateway.ToolsEnv)
	if !set {
		return nil, nil
	}
	profile, err := mcpgateway.ParseToolAllowlist(value)
	if err != nil {
		return nil, err
	}
	return []mcpgateway.ProfileSelection{{Source: "boot tools", Profile: profile}}, nil
}
