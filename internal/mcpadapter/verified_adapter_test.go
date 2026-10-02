package mcpadapter

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
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
