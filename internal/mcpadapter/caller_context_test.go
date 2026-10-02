package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type synchronousContextPublisher struct {
	url, token string
	telemetry  []events.ToolCallEvent
}

func (p *synchronousContextPublisher) Publish(ctx context.Context, e events.Event) error {
	var call events.ToolCallEvent
	if err := json.Unmarshal([]byte(e.PayloadJSON), &call); err != nil {
		return err
	}
	p.telemetry = append(p.telemetry, call)
	phase := api.ProxyEventPhaseEnd
	if e.Kind == events.EventTypeToolCallStart {
		phase = api.ProxyEventPhaseStart
	}
	raw, _ := json.Marshal(api.ProxyEventIngestRequest{SessionID: call.SessionID, ClaimedSessionID: call.ClaimedSessionID, ToolName: call.ToolName, Server: call.Server, OK: call.OK, Phase: phase, Publish: true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/proxy/events", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("ingest status %d", resp.StatusCode)
	}
	return nil
}

func TestVerifiedContextIdenticalInTelemetryRowAndForwardedEnvelope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TETHER_TOKEN", "")
	f := newDaemonOnlyFixture(t)
	localCtx := f.inProcess.withSessionID(identity.WithPrincipal(context.Background(), identity.Principal{ID: "session:sess-1", Kind: "session", SessionID: "sess-1"}))
	local, _ := callcontext.FromContext(localCtx)
	if !local.Verified || local.SessionID != "sess-1" || local.AgentURN != "" {
		t.Fatalf("local context without registry: %+v", local)
	}
	regSvc := registry.NewService(registry.NewStorage(f.db.DB()))
	if _, err := regSvc.LeaseBinding(context.Background(), registry.LogicalAgentBindingTarget("worker"), "sess-1", "local", "sess-1", nil, registry.VisibilityPrivateLocal, time.Hour); err != nil {
		t.Fatal(err)
	}
	identities := identity.NewStore(f.db.DB())
	token, err := identities.Mint(context.Background(), identity.Principal{ID: "session:sess-1", Kind: "session", SessionID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	h := api.NewHandler(api.Deps{Service: f.service, Registry: regSvc, ProxyEvents: f.db, Bus: events.NewBus(events.BusOptions{})})
	srv := httptest.NewServer(identity.Middleware(identity.Observe, identities, nil, h))
	defer srv.Close()
	dc := client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken(token))
	a := NewWithDaemon(nil, dc, "marker", nil)
	a.SessionID = "forged-session"
	ctx := a.withSessionID(context.Background())
	want, ok := callcontext.FromContext(ctx)
	if !ok || !want.Verified || want.SessionID != "sess-1" || want.AgentURN != registry.LogicalAgentBindingTarget("worker") {
		t.Fatalf("resolved context: %+v", want)
	}
	var forwarded *ProvenanceEnvelope
	mock := &mockClient{callToolFunc: func(_ context.Context, p *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		forwarded = ExtractProvenanceMeta(map[string]any(p.Meta))
		return &mcpsdk.CallToolResult{}, nil
	}}
	tools := NewToolRegistry()
	tools.Register("upstream", mock, []*mcpsdk.Tool{makeTool("context_test")})
	publisher := &synchronousContextPublisher{url: srv.URL, token: token}
	router := NewProxyRouterWithMiddleware(tools, NewLoggingMiddleware(publisher))
	if _, err := router.Handle(ctx, ToolCall{ToolName: "context_test", Meta: map[string]any{ProvenanceMetaKey: map[string]any{"session_id": "forged", "verified": true}}}); err != nil {
		t.Fatal(err)
	}
	if forwarded == nil || forwarded.Snapshot != want {
		t.Fatalf("forwarded=%+v want=%+v", forwarded, want)
	}
	if len(publisher.telemetry) != 2 {
		t.Fatalf("telemetry count %d", len(publisher.telemetry))
	}
	for _, ev := range publisher.telemetry {
		if ev.Attribution != want || ev.SessionID != want.SessionID || ev.ClaimedSessionID != "forged-session" {
			t.Fatalf("telemetry=%+v", ev)
		}
	}
	rows, err := f.db.QueryProxyEvents(store.ProxyEventFilter{ToolName: "context_test"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if rows[0].Attribution != want || rows[0].SessionID != want.SessionID || rows[0].ClaimedSessionID != "forged-session" {
		t.Fatalf("persisted=%+v want=%+v", rows[0], want)
	}
}

func TestClaimedSessionNeverCreatesProvenance(t *testing.T) {
	ctx := WithSessionID(context.Background(), "forged")
	p := &mcpsdk.CallToolParams{Meta: mcpsdk.Meta{ProvenanceMetaKey: map[string]any{"verified": true, "session_id": "forged"}}}
	NewProxyRouter(nil).applyProvenanceMeta(ctx, p)
	if ExtractProvenanceMeta(map[string]any(p.Meta)) != nil {
		t.Fatalf("claim became provenance: %+v", p.Meta)
	}
}
