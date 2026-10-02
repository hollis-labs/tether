// Package mcpforward relays a session's admitted daemon tools over stdio.
// It has no catalog, store, upstream pool, process spawning or local fallback.
package mcpforward

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type connectionTransport struct{ connection mcp.Connection }

type initialConnection struct {
	mcp.Connection
	initial jsonrpc.Message
}

func (c *initialConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	if c.initial != nil {
		msg := c.initial
		c.initial = nil
		return msg, nil
	}
	return c.Connection.Read(ctx)
}

func (t connectionTransport) Connect(context.Context) (mcp.Connection, error) {
	return t.connection, nil
}

// Run owns one downstream connection and one frozen-credential daemon session.
func Run(ctx context.Context, dc *client.Client, opts client.MCPOptions, transport mcp.Transport) error {
	downstream, err := transport.Connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = downstream.Close() }()
	first, err := downstream.Read(ctx)
	if err != nil {
		return err
	}
	downstream = &initialConnection{Connection: downstream, initial: first}
	notify := func(ctx context.Context, method string, params any) {
		data, marshalErr := json.Marshal(struct {
			JSONRPC string `json:"jsonrpc"`
			Method  string `json:"method"`
			Params  any    `json:"params"`
		}{"2.0", method, params})
		if marshalErr != nil {
			return
		}
		msg, decodeErr := jsonrpc.DecodeMessage(data)
		if decodeErr == nil {
			_ = downstream.Write(ctx, msg)
		}
	}
	opts.ClientOptions = &mcp.ClientOptions{
		Capabilities:   &mcp.ClientCapabilities{},
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
		ToolListChangedHandler: func(ctx context.Context, req *mcp.ToolListChangedRequest) {
			notify(ctx, "notifications/tools/list_changed", req.Params)
		},
		ProgressNotificationHandler: func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
			notify(ctx, "notifications/progress", req.Params)
		},
	}
	setup, cancel := context.WithTimeout(ctx, 2*time.Second)
	upstream, err := dc.ConnectMCP(setup, opts)
	cancel()
	if err != nil {
		failure := relayError(err)
		if req, ok := first.(*jsonrpc.Request); ok && req.IsCall() {
			_ = downstream.Write(ctx, &jsonrpc.Response{ID: req.ID, Error: failure})
		}
		return failure
	}
	defer func() { _ = upstream.Close() }()
	initialized := upstream.InitializeResult()
	server := mcp.NewServer(&mcp.Implementation{Name: "tether-forwarder", Version: "1"}, &mcp.ServerOptions{
		// The daemon Streamable HTTP transport currently negotiates legacy MCP.
		// Match that epoch: newer subscription envelopes require a separate relay.
		SupportedProtocolVersions: []string{"2025-11-25"},
		Instructions:              initialized.Instructions,
		Capabilities:              initialized.Capabilities,
	})
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "tools/list":
				params := mcp.ListToolsParams{}
				if incoming := req.GetParams().(*mcp.ListToolsParams); incoming != nil {
					params = *incoming
				}
				params.Meta = relayMeta(params.Meta)
				result, err := upstream.ListTools(ctx, &params)
				if err != nil {
					return nil, relayError(err)
				}
				return result, nil
			case "tools/call":
				raw := req.GetParams().(*mcp.CallToolParamsRaw)
				if raw == nil {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "tools/call requires parameters"}
				}
				result, err := upstream.CallTool(ctx, &mcp.CallToolParams{Meta: relayMeta(raw.Meta), Name: raw.Name, Arguments: raw.Arguments, InputResponses: raw.InputResponses, RequestState: raw.RequestState})
				if err != nil {
					return nil, relayError(err)
				}
				return result, nil
			case "ping":
				if err := upstream.Ping(ctx, nil); err != nil {
					return nil, relayError(err)
				}
			}
			return next(ctx, method, req)
		}
	})
	return server.Run(ctx, connectionTransport{downstream})
}

func relayError(err error) error {
	if errors.Is(err, client.ErrDaemonUnreachable) {
		return &jsonrpc.Error{Code: -32001, Message: "daemon_unreachable: start the daemon MCP endpoint or explicitly select legacy_proxy"}
	}
	// Protocol errors retain their structured category. Never expose transport
	// errors or response bodies which could echo a bearer credential.
	var protocol *jsonrpc.Error
	if errors.As(err, &protocol) {
		return protocol
	}
	return &jsonrpc.Error{Code: -32002, Message: "daemon_mcp_unavailable: endpoint disabled, credential rejected or connection lost"}
}

// Protocol negotiation belongs to each hop. Preserve application metadata and
// progress tokens while letting the daemon client stamp its negotiated version.
func relayMeta(meta mcp.Meta) mcp.Meta {
	forwarded := maps.Clone(meta)
	delete(forwarded, mcp.MetaKeyProtocolVersion)
	delete(forwarded, mcp.MetaKeyClientCapabilities)
	delete(forwarded, mcp.MetaKeyClientInfo)
	return forwarded
}
