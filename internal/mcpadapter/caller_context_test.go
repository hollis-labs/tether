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

	gomcp "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/settings"
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
	daemonServer := &daemon.Server{Config: daemon.Config{IdentityMode: identity.Observe}, Identity: identities, Service: f.service, Registry: regSvc, ProxyEvents: f.db, Bus: events.NewBus(events.BusOptions{Persister: f.db}), Settings: settings.NewService(settings.NewStorage(f.db.DB()))}
	defer daemonServer.CloseIdentityAudit()
	srv := httptest.NewServer(daemonServer.Handler())
	defer srv.Close()
	dc := client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken(token))
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/settings/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	settingsResponse, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = settingsResponse.Body.Close()
	if settingsResponse.StatusCode != http.StatusOK {
		t.Fatalf("production settings mount: %d", settingsResponse.StatusCode)
	}
	a := NewWithDaemon(nil, dc, "marker", nil)
	a.SessionID = "forged-session"
	ctx := a.withSessionID(context.Background())
	want, ok := callcontext.FromContext(ctx)
	if !ok || !want.Verified || want.SessionID != "sess-1" || want.AgentURN != registry.LogicalAgentBindingTarget("worker") {
		t.Fatalf("resolved context: %+v", want)
	}
	var forwarded *ContextEnvelope
	var legacy *ProvenanceEnvelope
	mock := &mockClient{callToolFunc: func(_ context.Context, p *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		forwarded = ExtractContextMeta(map[string]any(p.Meta))
		legacy = ExtractProvenanceMeta(map[string]any(p.Meta))
		return &mcpsdk.CallToolResult{}, nil
	}}
	tools := NewToolRegistry()
	tools.Register("upstream", mock, []*mcpsdk.Tool{makeTool("context_test")})
	publisher := &synchronousContextPublisher{url: srv.URL, token: token}
	router := NewProxyRouterWithMiddleware(tools, NewLoggingMiddleware(publisher))
	if _, err := router.Handle(ctx, ToolCall{ToolName: "context_test", Meta: map[string]any{ProvenanceMetaKey: map[string]any{"session_id": "forged", "verified": true}, ContextMetaKey: map[string]any{"schema_version": 2, "session_id": "forged", "verified": true, "source": "daemon"}}}); err != nil {
		t.Fatal(err)
	}
	if forwarded == nil || forwarded.Snapshot != want {
		t.Fatalf("forwarded=%+v want=%+v", forwarded, want)
	}
	if legacy == nil || legacy.SchemaVersion != 1 || legacy.SessionID != want.SessionID || legacy.WorkstreamID != want.WorkstreamID {
		t.Fatalf("production schema-1 stamp=%+v want session=%s workstream=%s", legacy, want.SessionID, want.WorkstreamID)
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
	readback, err := (DaemonProxyEvents{Client: dc}).QueryProxyEvents(store.ProxyEventFilter{ToolName: "context_test"})
	if err != nil || len(readback) != 1 || readback[0].Attribution != want || readback[0].ClaimedSessionID != "forged-session" {
		t.Fatalf("daemon readback=%+v err=%v", readback, err)
	}
}

func TestClaimedSessionNeverCreatesTrustedContext(t *testing.T) {
	ctx := WithSessionID(context.Background(), "forged")
	p := &mcpsdk.CallToolParams{Meta: mcpsdk.Meta{ProvenanceMetaKey: map[string]any{"verified": true, "session_id": "forged"}, ContextMetaKey: map[string]any{"verified": true, "source": "daemon", "session_id": "forged"}}}
	NewProxyRouter(nil).applyProvenanceMeta(ctx, p)
	if ExtractContextMeta(map[string]any(p.Meta)) != nil {
		t.Fatalf("claim became trusted context: %+v", p.Meta)
	}
}

func TestLegacyProvenanceShapeUnchangedAlongsideTrustedContext(t *testing.T) {
	for _, verified := range []bool{false, true} {
		t.Run(fmt.Sprint(verified), func(t *testing.T) {
			ctx := WithSessionID(context.Background(), "s")
			if verified {
				ctx = callcontext.WithSnapshot(ctx, callcontext.Snapshot{Verified: true, Source: "daemon", PrincipalID: "session:s", SessionID: "s", WorkstreamID: "w"})
			}
			router := NewProxyRouter(nil)
			router.SetWorkstreamResolver(func(context.Context, string) (string, error) { return "w", nil })
			params := &mcpsdk.CallToolParams{Meta: mcpsdk.Meta{ProvenanceMetaKey: map[string]any{"session_id": "spoof"}, ContextMetaKey: map[string]any{"verified": true, "source": "daemon", "session_id": "spoof"}}}
			router.applyProvenanceMeta(ctx, params)
			// Exact bytes emitted by main's schema-1 serializer: no added fields.
			raw, err := json.Marshal(params.Meta[ProvenanceMetaKey])
			if err != nil || string(raw) != `{"schema_version":1,"session_id":"s","workstream_id":"w"}` {
				t.Fatalf("legacy envelope=%s err=%v", raw, err)
			}
			trusted := ExtractContextMeta(map[string]any(params.Meta))
			if !verified && trusted != nil {
				t.Fatalf("unverified context promoted: %+v", trusted)
			}
			if verified && (trusted == nil || trusted.SchemaVersion != 2 || !trusted.Verified || trusted.Source != "daemon" || trusted.SessionID != "s" || trusted.WorkstreamID != "w") {
				t.Fatalf("trusted context=%+v", trusted)
			}
		})
	}
}

func TestCallerContextSlowDaemonIsBoundedAndNegativelyCached(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := NewWithDaemon(nil, nil, "marker", nil)
	var lookups int
	a.SetCallerContextResolver(func(ctx context.Context) (callcontext.Snapshot, error) {
		lookups++
		<-ctx.Done()
		return callcontext.Snapshot{}, ctx.Err()
	})
	s := a.newBareServer()
	a.addTool(s, gomcp.Tool{Name: "noop", Description: "Local noop", InputSchema: gomcp.EmptyObjectSchema(), Handler: func(context.Context, map[string]any) (any, error) { return "ok", nil }}, Reads("local noop"))
	start := time.Now()
	if _, err := s.CallTool(context.Background(), "noop", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if lookups != 0 || time.Since(start) > 100*time.Millisecond {
		t.Fatal("native noop performed attribution lookup")
	}
	start = time.Now()
	ctx := a.withSessionID(context.Background())
	if snapshot, _ := callcontext.FromContext(ctx); snapshot.Verified {
		t.Fatal("failed lookup stamped context")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("lookup exceeded bounded overhead")
	}
	start = time.Now()
	for range 10 {
		a.withSessionID(context.Background())
	}
	if lookups != 1 || time.Since(start) > 100*time.Millisecond {
		t.Fatal("failed lookup was not negatively cached")
	}
	a.SetCallerContextResolver(func(context.Context) (callcontext.Snapshot, error) {
		lookups++
		return callcontext.Snapshot{Verified: true, Source: "daemon", PrincipalID: "session:A", SessionID: "A"}, nil
	})
	for range 10 {
		a.withSessionID(context.Background())
	}
	if lookups != 2 {
		t.Fatal("positive lookup was not cached")
	}
}

func TestReservedContextMetaCaseVariantsAreStripped(t *testing.T) {
	p := &mcpsdk.CallToolParams{Meta: mcpsdk.Meta{"Tether.Context": map[string]any{"verified": true}, "TETHER.PROVENANCE": map[string]any{"session_id": "forged"}, "ordinary": "kept"}}
	NewProxyRouter(nil).applyProvenanceMeta(context.Background(), p)
	if len(p.Meta) != 1 || p.Meta["ordinary"] != "kept" {
		t.Fatal("case-variant reserved envelope survived")
	}
}
