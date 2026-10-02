package mcpadapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"

	gomcpclient "github.com/hollis-labs/go-mcp/client"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type progressSessionKey struct{}

type progressRoute struct {
	origin  string
	session *mcpsdk.ServerSession
	token   any
	ctx     context.Context
}

// Shared upstream tokens belong to a single call, never to a caller-selected
// token shared across views. This router exists only in the daemon-owned pool.
type sharedProgress struct {
	mu     sync.Mutex
	routes map[string]progressRoute
	redact func(string) string
}

func (p *sharedProgress) bind(ctx context.Context, origin string, params *mcpsdk.CallToolParams) func() {
	session, _ := ctx.Value(progressSessionKey{}).(*mcpsdk.ServerSession)
	token := params.GetProgressToken()
	if session == nil || token == nil {
		return func() {}
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		// Fail closed for notification routing; never forward an ambiguous token.
		delete(params.Meta, "progressToken")
		return func() {}
	}
	hop := hex.EncodeToString(nonce[:])
	routeCtx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.routes[hop] = progressRoute{origin: origin, session: session, token: token, ctx: routeCtx}
	p.mu.Unlock()
	params.Meta["progressToken"] = hop
	return func() { cancel(); p.mu.Lock(); delete(p.routes, hop); p.mu.Unlock() }
}

func (p *sharedProgress) notify(origin string, req *mcpsdk.ProgressNotificationClientRequest) {
	hop, ok := req.Params.ProgressToken.(string)
	if !ok {
		return
	}
	p.mu.Lock()
	route, found := p.routes[hop]
	p.mu.Unlock()
	if !found || route.origin != origin || route.ctx.Err() != nil {
		return
	}
	// Upstream metadata is not a trusted caller envelope. Only the protocol's
	// progress fields pass through, with catalog credentials scrubbed.
	params := &mcpsdk.ProgressNotificationParams{ProgressToken: route.token, Progress: req.Params.Progress, Total: req.Params.Total, Message: p.redact(req.Params.Message)}
	_ = route.session.NotifyProgress(route.ctx, params)
}

func (p *ClientPool) remoteProgressOptions() []gomcpclient.Option {
	if p.progress == nil {
		return nil
	}
	return []gomcpclient.Option{gomcpclient.WithClientOptions(func(origin string, opts *mcpsdk.ClientOptions) {
		opts.ProgressNotificationHandler = func(_ context.Context, req *mcpsdk.ProgressNotificationClientRequest) { p.progress.notify(origin, req) }
	})}
}
