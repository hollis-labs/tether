package teamruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

type sessionAnswer func(context.Context, string) (mesh.URN, error)

func (answer sessionAnswer) SessionActor(ctx context.Context, id string) (mesh.URN, error) {
	return answer(ctx, id)
}
func TestPrincipalRefusalPrecedesEnrollmentSessionLookup(t *testing.T) {
	calls := 0
	source := sessionAnswer(func(_ context.Context, id string) (mesh.URN, error) {
		calls++
		if id != "authenticated" {
			t.Fatal("unretained selector", id)
		}
		return "msg://agent/local/retained", nil
	})
	resolver := Principals{Mode: identity.Enforce, Sessions: source}
	if _, err := resolver.ResolvePrincipal(context.Background()); !errors.Is(err, teamsvc.ErrUnauthenticated) {
		t.Fatal("missing identity admitted", err)
	}
	verified := identity.WithPrincipal(ctx, identity.Principal{ID: "msg://session/local/authenticated", Kind: "session", SessionID: "authenticated"})
	resolver.Mode = identity.Observe
	if _, err := resolver.ResolvePrincipal(verified); !errors.Is(err, teamsvc.ErrUnauthenticated) {
		t.Fatal("observe attributed identity admitted", err)
	}
	if calls != 0 {
		t.Fatal("enrollment adapter received an unauthenticated lookup", calls)
	}
	resolver.Mode = identity.Enforce
	out, err := resolver.ResolvePrincipal(verified)
	check(t, err)
	if calls != 1 || out.ID != "msg://agent/local/retained" || !out.Verified {
		t.Fatal("authenticated seam failed", out, calls)
	}
	resolver.Sessions = sessionAnswer(func(context.Context, string) (mesh.URN, error) { return "", nil })
	if _, err = resolver.ResolvePrincipal(verified); !errors.Is(err, teamsvc.ErrUnauthenticated) {
		t.Fatal("adapter omission became admission", err)
	}
}
