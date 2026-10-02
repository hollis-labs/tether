package mcpadapter

import (
	"context"
	"errors"

	gomcp "github.com/hollis-labs/go-mcp/server"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

func (a *Adapter) channelCallerContext(ctx context.Context) context.Context {
	if a.principal != nil {
		return identity.WithPrincipal(ctx, *a.principal)
	}
	return ctx
}

func (a *Adapter) channelConsumers() app.ConsumerService {
	// A standalone adapter reads through the daemon, preserving its verified
	// caller context and policy. Daemon-native views use the same local service.
	if a.client != nil {
		return app.ConsumerService{ChannelReader: a.client, RoutingReader: a.client}
	}
	if a.svc != nil && a.svc.Store != nil && a.principal != nil {
		return app.ConsumerService{ChannelReader: channels.New(a.svc.Store, nil), RoutingReader: a.svc}
	}
	return app.ConsumerService{}
}

func (a *Adapter) registerChannelTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name:        "tether_channel_list",
		Description: "List named channels with derived addresses; no membership is required.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("as", "Caller identity URN; verified identity takes precedence", false),
			gomcp.NumberProp("limit", "Channels per page (1..1000, default 100)", false),
			gomcp.NumberProp("offset", "Channel offset (default 0)", false),
		),
		Handler: a.handleChannelList,
	}, Reads("GET /channels; channel history is never consumed"))
	a.addTool(s, gomcp.Tool{
		Name:        "tether_channel_read",
		Description: "Read durable channel history oldest first. Resume with next_since; purged messages carry tombstones. SSE subscriptions use HTTP.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("name", "Stable channel name", true),
			gomcp.StringProp("as", "Caller identity URN; verified identity takes precedence", false),
			gomcp.NumberProp("since", "Exclusive publication sequence (default 0)", false),
			gomcp.NumberProp("limit", "Messages per page (1..1000, default 100)", false),
			gomcp.NumberProp("last", "Read recent N messages (1..1000); excludes since and limit", false),
		),
		Handler: a.handleChannelRead,
	}, Reads("GET /channels/{name}/messages; no mailbox mutations"))
	a.addTool(s, gomcp.Tool{
		Name:        "tether_routing_get",
		Description: "Get installed routing capabilities for the gateway or a session. Per-output final-text confidence is authoritative.",
		InputSchema: gomcp.InputSchema(gomcp.StringProp("session_id", "Optional Tether session id", false)),
		Handler:     a.handleRoutingGet,
	}, Reads("GET /routing/capabilities"))
}

func channelConsumerError(err error) error {
	if errors.Is(err, channels.ErrInvalid) {
		return toolError("invalid_request", err.Error())
	}
	if errors.Is(err, channels.ErrForbidden) {
		return toolError("forbidden", err.Error())
	}
	if errors.Is(err, app.ErrRoutingSessionNotFound) {
		return toolError("not_found", err.Error())
	}
	if isDaemonUnreachable(err) {
		return daemonUnreachableError(err)
	}
	return classifyClientErr(err, "")
}

func (a *Adapter) handleChannelList(ctx context.Context, args map[string]any) (any, error) {
	ctx = a.channelCallerContext(ctx)
	page, err := a.channelConsumers().ListPage(ctx, str(args, "as"), intArg(args, "offset", 0), intArg(args, "limit", 0))
	if err != nil {
		return nil, channelConsumerError(err)
	}
	return toolJSON(page), nil
}

func (a *Adapter) handleChannelRead(ctx context.Context, args map[string]any) (any, error) {
	ctx = a.channelCallerContext(ctx)
	page, err := a.channelConsumers().Read(ctx, str(args, "name"), str(args, "as"), int64(intArg(args, "since", 0)), intArg(args, "limit", 0), intArg(args, "last", 0))
	if err != nil {
		return nil, channelConsumerError(err)
	}
	return toolJSON(page), nil
}

func (a *Adapter) handleRoutingGet(ctx context.Context, args map[string]any) (any, error) {
	consumer := a.channelConsumers()
	if consumer.RoutingReader == nil {
		return nil, toolError("internal_error", "routing consumer requires daemon routing")
	}
	result, err := consumer.RoutingCapabilities(ctx, str(args, "session_id"))
	if err != nil {
		return nil, channelConsumerError(err)
	}
	return toolJSON(result), nil
}
