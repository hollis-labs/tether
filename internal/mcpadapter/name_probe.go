package mcpadapter

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/redact"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ProbeNames starts catalog upstreams solely for initialize/tools/list. It uses
// the normal spawn environment scrub, authored wrappers, confinement and naming
// boundary. The explicit doctor opt-in owns the probe child lifetimes.
func (a *Adapter) ProbeNames(ctx context.Context, catalogDir string, opts ProxyOptions) (mcpgateway.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	authored, err := config.LoadMCPServerCatalog(catalogDir)
	if err != nil {
		return mcpgateway.Status{}, fmt.Errorf("cannot read MCP catalog for live name probe")
	}
	ids := opts.ServerFilter
	if ids == nil {
		ids = []string{}
		if !opts.Confine {
			for _, entry := range authored {
				if entry.IsEnabled() {
					ids = append(ids, entry.ID)
				}
			}
		}
	}
	ids, err = mcpgateway.SelectOrigins(authoredOriginStates(authored), ids, nil)
	if err != nil {
		return mcpgateway.Status{}, err
	}
	entries, unknown, err := config.LoadMCPServersConfinedContext(ctx, catalogDir, ids)
	if err != nil {
		return mcpgateway.Status{}, fmt.Errorf("MCP live probe credential resolution failed; check credential configuration")
	}
	if len(unknown) > 0 {
		return mcpgateway.Status{}, fmt.Errorf("unknown or disabled MCP servers in live probe")
	}
	registry := NewToolRegistry()
	pool := NewClientPool(entries, registry)
	pool.probe = true
	pool.confineRemote = len(a.protected) > 0 && os.Getenv(config.MCPConfineRemoteEnv) == "1"
	pool.runtime = a.runtime
	a.upstreams = pool
	defer pool.Shutdown()
	native := a.newServer()
	a.registerCatalogRefreshTool(native, pool)
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	ss, err := native.SDKServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		return mcpgateway.Status{}, err
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "tether-name-probe", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		return mcpgateway.Status{}, err
	}
	defer func() { _ = cs.Close() }()
	for tool, listErr := range cs.Tools(ctx, nil) {
		if listErr != nil {
			return mcpgateway.Status{}, listErr
		}
		if err := registry.RegisterLocal(tool, cs); err != nil {
			return mcpgateway.Status{}, err
		}
	}
	startErr := pool.Start(ctx)
	gateway := a.gatewayService(registry, NewProxyRouter(registry), mcpgateway.Selection{Mode: mcpgateway.Flat, Source: "doctor_live_probe"}, nil)
	status := gateway.Status("")
	// Remove every known credential/URL from the typed diagnostic strings before
	// JSON encoding, so escaping cannot make a raw value evade redaction.
	secrets := probeRedactor(entries)
	for i := range status.Origins {
		status.Origins[i].ID = redactProbeText(secrets, status.Origins[i].ID)
		status.Origins[i].Error = redactProbeText(secrets, status.Origins[i].Error)
	}
	for i := range status.Lint {
		f := &status.Lint[i]
		f.Origin = redactProbeText(secrets, f.Origin)
		f.Name = redactProbeText(secrets, f.Name)
		f.Message = redactProbeText(secrets, f.Message)
	}
	for i := range status.Collisions {
		c := &status.Collisions[i]
		c.Name = redactProbeText(secrets, c.Name)
		for j := range c.Owners {
			o := &c.Owners[j]
			o.Origin = redactProbeText(secrets, o.Origin)
			o.Name = redactProbeText(secrets, o.Name)
		}
	}
	if len(status.Collisions) > 0 {
		return status, &mcpgateway.CollisionError{Collisions: status.Collisions}
	}
	if ctx.Err() != nil {
		return status, fmt.Errorf("MCP live name probe reached its overall timeout")
	}
	return status, startErr
}
func reservedOriginError() error {
	return fmt.Errorf("MCP upstream origin %q is reserved for native Tether tools; rename the catalog upstream ID", "tether")
}
func probeRedactor(entries []config.MCPServerEntry) *redact.Set {
	out := proxyRedactionSet(entries)
	for _, entry := range entries {
		out.Add(entry.URL)
		if endpoint, err := url.Parse(entry.URL); err == nil {
			out.Add(endpoint.Hostname())
			out.Add(endpoint.Host)
		}
		for i, arg := range entry.Args {
			if i > 0 {
				switch entry.Args[i-1] {
				case "--token", "--api-key", "--secret", "--password":
					out.Add(arg)
				}
			}
			for _, prefix := range []string{"--token=", "--api-key=", "--secret=", "--password="} {
				if strings.HasPrefix(arg, prefix) {
					out.Add(strings.TrimPrefix(arg, prefix))
				}
			}
			if strings.Contains(arg, "://") {
				out.Add(arg)
			}
		}
		if entry.URL != "" {
			withoutScheme := strings.TrimPrefix(strings.TrimPrefix(entry.URL, "https://"), "http://")
			out.Add(withoutScheme)
		}
	}
	return out
}

var diagnosticURL = regexp.MustCompile(`(?i)(?:https?|sse)://[^\s"<>]+`)

func redactProbeText(secrets *redact.Set, text string) string {
	return diagnosticURL.ReplaceAllString(secrets.Redact(text), redact.Marker)
}
