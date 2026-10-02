// Package mcpforward relays a session's admitted daemon tools over stdio.
// It has no catalog, store, upstream pool, process spawning or local fallback.
package mcpforward

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

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
	relay := &daemonSession{client: dc, opts: opts}
	upstream, err := relay.get(ctx, nil)
	if err != nil {
		failure := relayError(err)
		if req, ok := first.(*jsonrpc.Request); ok && req.IsCall() {
			_ = downstream.Write(ctx, &jsonrpc.Response{ID: req.ID, Error: failure})
		}
		return failure
	}
	defer relay.close()
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
				result, err := relay.invokeRead(ctx, func(upstream *mcp.ClientSession) (mcp.Result, error) {
					return upstream.ListTools(ctx, &params)
				})
				if err != nil {
					return nil, relayError(err)
				}
				return result, nil
			case "tools/call":
				raw := req.GetParams().(*mcp.CallToolParamsRaw)
				if raw == nil {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "tools/call requires parameters"}
				}
				result, err := relay.callTool(ctx, &mcp.CallToolParams{Meta: relayMeta(raw.Meta), Name: raw.Name, Arguments: raw.Arguments, InputResponses: raw.InputResponses, RequestState: raw.RequestState})
				if err != nil {
					return nil, relayError(err)
				}
				return result, nil
			case "ping":
				if _, err := relay.invokeRead(ctx, func(upstream *mcp.ClientSession) (mcp.Result, error) {
					return nil, upstream.Ping(ctx, nil)
				}); err != nil {
					return nil, relayError(err)
				}
			}
			return next(ctx, method, req)
		}
	})
	return server.Run(ctx, connectionTransport{downstream})
}

// daemonSession serializes initialization, not tool execution. The credential,
// profile and discovery selector remain frozen through every replacement view.
type daemonSession struct {
	mu      sync.Mutex
	client  *client.Client
	opts    client.MCPOptions
	session *mcp.ClientSession
	done    <-chan struct{}
	missing *atomic.Bool
}

func (d *daemonSession) get(ctx context.Context, missing *mcp.ClientSession) (*mcp.ClientSession, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session != nil && d.session != missing {
		select {
		case <-d.done:
			// A closed previous transport cannot execute a new request. Reconnect
			// before dispatch, without replaying any previous uncertain outcome.
		default:
			if !d.missing.Load() {
				return d.session, nil
			}
		}
	}
	if d.session != nil {
		_ = d.session.Close()
		d.session = nil
	}
	setup, cancel := context.WithTimeout(ctx, client.MCPInitializeTimeout)
	defer cancel()
	missingState := &atomic.Bool{}
	options := d.opts
	options.OnSessionMissing = func() { missingState.Store(true) }
	session, err := d.client.ConnectMCP(setup, options)
	if err != nil {
		if setup.Err() != nil {
			return nil, setup.Err()
		}
		return nil, err
	}
	d.session = session
	d.missing = missingState
	done := make(chan struct{})
	d.done = done
	go func() { _ = session.Wait(); close(done) }()
	return session, nil
}

func (d *daemonSession) invokeRead(ctx context.Context, call func(*mcp.ClientSession) (mcp.Result, error)) (mcp.Result, error) {
	session, err := d.get(ctx, nil)
	if err != nil {
		return nil, err
	}
	result, err := call(session)
	// Missing-session errors may originate from a background stream and retire
	// unrelated in-flight requests too. Retry only these read-only list/ping
	// operations. Transport loss can also reconnect a read once; cancellation
	// and protocol/admission errors are never retried.
	if err == nil {
		return result, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	d.mu.Lock()
	observedMissing := d.session == session && d.missing.Load()
	d.mu.Unlock()
	if !observedMissing {
		// A closed transport can race our Wait observer. Read-only operations may
		// reconnect once, but generic/unknown 404 and protocol errors must surface.
		if errors.Is(err, mcp.ErrSessionMissing) || strings.Contains(err.Error(), "session not found") || strings.Contains(err.Error(), "Not Found") {
			return result, err
		}
		var protocol *jsonrpc.Error
		if errors.As(err, &protocol) {
			return result, err
		}
		failure := relayError(err)
		if failure.Code != -32001 {
			return result, err
		}
	}
	session, err = d.get(ctx, session)
	if err != nil {
		return nil, err
	}
	return call(session)
}

// Probe before tool dispatch so idle views recover without ever replaying a
// mutation. The view can still disappear during execution: that outcome is
// unknown and belongs to the caller, regardless of the SDK error category.
func (d *daemonSession) callTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if _, err := d.invokeRead(ctx, func(session *mcp.ClientSession) (mcp.Result, error) {
		return nil, session.Ping(ctx, nil)
	}); err != nil {
		return nil, err
	}
	session, err := d.get(ctx, nil)
	if err != nil {
		return nil, err
	}
	result, err := session.CallTool(ctx, params)
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return result, err
}

func (d *daemonSession) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session != nil {
		_ = d.session.Close()
	}
}

func relayError(err error) *jsonrpc.Error {
	// Transport failures may also wrap the SDK's ErrRejected protocol sentinel.
	// Classify that typed cause before preserving an explicit daemon response.
	if daemonTransportFailure(err) {
		return &jsonrpc.Error{Code: -32001, Message: "daemon_unreachable: daemon connection lost; tool outcome may be unknown"}
	}
	var protocol *jsonrpc.Error
	if errors.As(err, &protocol) {
		return protocol
	}
	if errors.Is(err, context.Canceled) {
		return &jsonrpc.Error{Code: -32003, Message: "daemon_mcp_canceled: request canceled; outcome may be unknown"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &jsonrpc.Error{Code: -32004, Message: "daemon_mcp_timeout: request deadline exceeded; cold upstream initialization may need more time"}
	}
	// Never expose transport errors or response bodies which could echo a token.
	return &jsonrpc.Error{Code: -32002, Message: "daemon_mcp_unavailable: daemon did not accept this MCP request"}
}

func daemonTransportFailure(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, client.ErrDaemonUnreachable) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var operation *net.OpError
	var request *url.Error
	if errors.As(err, &operation) || errors.As(err, &request) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var network net.Error
	if errors.As(err, &network) {
		return true
	}
	var protocol *jsonrpc.Error
	if errors.As(err, &protocol) {
		return false
	}
	// The SDK synthesizes this untyped error when an HTTP/SSE call ends before
	// receiving a response, and formats body/reconnect failures with %v. These
	// transport failures cannot retain an errors.Is sentinel through the SDK.
	detail := err.Error()
	if strings.Contains(detail, "session not found") || strings.Contains(detail, "Not Found") || strings.Contains(detail, "Forbidden") || strings.Contains(detail, "Unauthorized") {
		return false
	}
	for _, marker := range []string{"daemon unreachable", "request terminated without response", "failed to read body:", "failed to reconnect (session ID:", "client is closing", "connection refused", "connection reset", "use of closed network connection", "unexpected EOF", "i/o timeout"} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return strings.Contains(detail, "standalone SSE stream: exceeded ") && strings.Contains(detail, " retries without progress (session ID:")
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
