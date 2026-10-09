package mcpadapter

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	gomcp "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestVerifiedAdapter_RefusesBroaderNativeClientCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	db, err := store.Open(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ids := identity.NewStore(db.DB())
	p := identity.Principal{ID: "restricted", Kind: "service", Scopes: []string{"message.write"}}
	callerToken, err := ids.Mint(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	opToken, err := ids.Mint(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.WriteTokenFile(filepath.Join(home, ".tether", "run", "operator.token"), opToken); err != nil {
		t.Fatal(err)
	}
	ctx := identity.WithPrincipal(context.Background(), p)
	svc := &app.Service{Store: db}
	for _, dc := range []*client.Client{client.New("unix:"+filepath.Join(home, "unused.sock"), client.WithToken(opToken)), client.New("unix:" + filepath.Join(home, "unused.sock"))} {
		// Changing environment after construction cannot conceal the frozen
		// operator credential the client's actual transport will send.
		t.Setenv("TETHER_TOKEN", callerToken)
		if _, err := NewVerifiedAdapter(ctx, svc, dc); !errors.Is(err, identity.ErrInvalidToken) {
			t.Fatal("operator API client accepted for restricted view", err)
		}
	}
	dc := client.New("unix:"+filepath.Join(home, "unused.sock"), client.WithToken(callerToken))
	if _, err := NewVerifiedAdapter(ctx, svc, dc); err != nil {
		t.Fatal("matching native credential refused", err)
	}
}

func TestVerifiedAdapter_UsesOnlyVerifiedSessionAndScopes(t *testing.T) {
	p := identity.Principal{ID: "verified", Kind: "session", SessionID: "actual", Scopes: []string{"message.write"}}
	a, err := NewVerifiedAdapter(identity.WithPrincipal(context.Background(), p), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.SessionID = "client-forged"
	ctx := a.withSessionID(identity.WithPrincipal(WithSessionID(context.Background(), "forged"), identity.Principal{ID: "imposter"}))
	verified, ok := identity.FromContext(ctx)
	if !ok || verified.ID != "verified" || sessionIDFromContext(ctx) != "actual" {
		t.Fatalf("identity overwritten: %+v %s", verified, sessionIDFromContext(ctx))
	}
	if err := a.checkScope(ScopeMessageWrite); err != nil {
		t.Fatal(err)
	}
	if err := a.checkScope(ScopeSessionWrite); err == nil {
		t.Fatal("client widened verified scopes")
	}
	if _, err := NewVerifiedAdapter(context.Background(), nil, nil); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("unverified constructor accepted", err)
	}
}

func TestVerifiedAdapter_OperatorWildcardDoesNotAlterLegacyScopes(t *testing.T) {
	p := identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	a, err := NewVerifiedAdapter(identity.WithPrincipal(context.Background(), p), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.checkScope(ScopeSessionWrite); err != nil {
		t.Fatal(err)
	}
	legacy := New(nil, "legacy", []string{"*"})
	if err := legacy.checkScope(ScopeSessionWrite); err == nil {
		t.Fatal("legacy caller flags widened")
	}
}

func TestVerifiedAdapter_NativeSDKResolvesAdmittedCallerContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := newDaemonOnlyFixture(t)
	p := identity.Principal{ID: "session:sess-1", Kind: "session", SessionID: "sess-1"}
	a, err := NewVerifiedAdapter(identity.WithPrincipal(context.Background(), p), f.inProcess.svc, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.SessionID = "forged-session"
	seen := make(chan callcontext.Snapshot, 1)
	server := a.newBareServer()
	a.addTool(server, gomcp.Tool{Name: "test_native_context", Description: "test", InputSchema: gomcp.InputSchema(), Handler: func(ctx context.Context, _ map[string]any) (any, error) {
		snapshot, _ := callcontext.FromContext(ctx)
		seen <- snapshot
		return "ok", nil
	}}, Reads("test native attribution"))
	forged := callcontext.WithSnapshot(identity.WithPrincipal(context.Background(), identity.Principal{ID: "imposter"}), callcontext.Snapshot{Verified: true, Source: "daemon", PrincipalID: "imposter", SessionID: "forged-session"})
	st, ct := mcpsdk.NewInMemoryTransports()
	ss, err := server.SDKServer().Connect(forged, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil).Connect(forged, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	if _, err := cs.CallTool(forged, &mcpsdk.CallToolParams{Name: "test_native_context"}); err != nil {
		t.Fatal(err)
	}
	got := <-seen
	if !got.Verified || got.Source != "daemon" || got.PrincipalID != p.ID || got.SessionID != p.SessionID {
		t.Fatalf("native SDK trusted forged attribution: %+v", got)
	}
}
