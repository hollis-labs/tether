package acpadapter

import (
	"encoding/json"
	"testing"
)

func TestAuthGate_NoTokenIsAuthedByDefault(t *testing.T) {
	g := NewAuthGate("", nil)
	if !g.IsAuthed() {
		t.Fatal("empty-token gate should pre-authenticate (dev mode)")
	}
	if g.AuthMethods() != nil {
		t.Errorf("AuthMethods should be nil when auth disabled, got %+v", g.AuthMethods())
	}
}

func TestAuthGate_TokenRequiredBeforeScope(t *testing.T) {
	g := NewAuthGate("secret", []string{ScopeSessionWrite})
	if g.IsAuthed() {
		t.Fatal("gate should not be authed before authenticate")
	}
	if rpcErr := g.HasScope(ScopeSessionWrite); rpcErr == nil {
		t.Fatal("HasScope should fail before authenticate")
	}
}

func TestAuthGate_AuthenticateValidatesMethodAndToken(t *testing.T) {
	g := NewAuthGate("secret", []string{ScopeSessionWrite})

	// Wrong method id.
	if err := g.Authenticate(AuthenticateParams{MethodID: "oauth"}); err == nil {
		t.Fatal("expected error for unknown method")
	}

	// Right method, wrong token.
	body, _ := json.Marshal(AuthenticateTokenBody{Token: "wrong"})
	if err := g.Authenticate(AuthenticateParams{MethodID: "token", Body: body}); err == nil {
		t.Fatal("expected error for wrong token")
	}

	// Right method, right token.
	body, _ = json.Marshal(AuthenticateTokenBody{Token: "secret"})
	if err := g.Authenticate(AuthenticateParams{MethodID: "token", Body: body}); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if !g.IsAuthed() {
		t.Fatal("gate should be authed after successful authenticate")
	}
}

func TestAuthGate_HasScopeAfterAuth(t *testing.T) {
	g := NewAuthGate("secret", []string{ScopeSessionWrite})
	body, _ := json.Marshal(AuthenticateTokenBody{Token: "secret"})
	_ = g.Authenticate(AuthenticateParams{MethodID: "token", Body: body})

	if rpcErr := g.HasScope(ScopeSessionWrite); rpcErr != nil {
		t.Errorf("granted scope rejected: %v", rpcErr)
	}
	if rpcErr := g.HasScope("nonexistent"); rpcErr == nil {
		t.Error("ungranted scope accepted")
	}
}

func TestAuthGate_AuthMethodsAdvertisesToken(t *testing.T) {
	g := NewAuthGate("secret", nil)
	methods := g.AuthMethods()
	if len(methods) != 1 {
		t.Fatalf("AuthMethods len = %d, want 1", len(methods))
	}
	if methods[0].ID != "token" {
		t.Errorf("auth method id = %q, want token", methods[0].ID)
	}
}
