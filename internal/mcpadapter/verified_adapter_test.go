package mcpadapter

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

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
