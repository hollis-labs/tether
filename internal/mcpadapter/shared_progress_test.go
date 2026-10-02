package mcpadapter

import (
	"context"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSharedProgressRejectsWrongOriginAndInactiveRoutes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &sharedProgress{routes: map[string]progressRoute{}, redact: func(s string) string { return s }}
	// A nil session intentionally makes any attempted delivery fail the test.
	// The valid-delivery path is exercised with real SDK sessions by transport tests.
	p.routes["active-hop"] = progressRoute{origin: "alpha", ctx: ctx}
	notification := func(token any) *mcpsdk.ProgressNotificationClientRequest {
		return &mcpsdk.ProgressNotificationClientRequest{Params: &mcpsdk.ProgressNotificationParams{ProgressToken: token, Progress: 1}}
	}
	p.notify("beta", notification("active-hop"))
	p.notify("alpha", notification("unknown-hop"))
	p.notify("alpha", notification(123))
	cancel()
	p.notify("alpha", notification("active-hop"))
	delete(p.routes, "active-hop")
	p.notify("alpha", notification("active-hop"))
}

func TestSharedProgressBindReplacesTokenAndReleasesRoute(t *testing.T) {
	p := &sharedProgress{routes: map[string]progressRoute{}, redact: func(s string) string { return s }}
	ctx := context.WithValue(context.Background(), progressSessionKey{}, &mcpsdk.ServerSession{})
	params := &mcpsdk.CallToolParams{Meta: mcpsdk.Meta{"progressToken": "caller-token"}}
	finish := p.bind(ctx, "alpha", params)
	hop, ok := params.GetProgressToken().(string)
	if !ok || hop == "caller-token" || len(hop) != 64 {
		t.Fatalf("unsafe upstream token %v", params.GetProgressToken())
	}
	route, exists := p.routes[hop]
	if !exists || route.origin != "alpha" || route.token != "caller-token" || route.ctx.Err() != nil {
		t.Fatal("call route not bound to its origin and original caller token")
	}
	finish()
	if _, exists := p.routes[hop]; exists || route.ctx.Err() == nil {
		t.Fatal("completed route remains deliverable")
	}
	p.notify("alpha", &mcpsdk.ProgressNotificationClientRequest{Params: &mcpsdk.ProgressNotificationParams{ProgressToken: hop, Progress: 1}})
}
