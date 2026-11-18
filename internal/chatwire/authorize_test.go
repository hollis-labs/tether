package chatwire

import (
	"context"
	"errors"
	"testing"

	chat "github.com/hollis-labs/libs/agent/chat"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestAuthorize(t *testing.T) {
	local := context.Background()
	as := func(p identity.Principal) context.Context { return identity.WithPrincipal(local, p) }
	operator := as(identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"})
	reader := as(identity.Principal{Verified: true, ID: "dev-1", Kind: "device", Scopes: []string{identity.ScopeRead}})
	operate := as(identity.Principal{Verified: true, ID: "dev-2", Kind: "device", Scopes: []string{identity.ScopeRead, identity.ScopeOperate}})
	claimed := as(identity.Principal{ID: "someone", Kind: "device"}) // not verified: a claim
	cases := []struct {
		name  string
		ctx   context.Context
		owner string
		perm  chat.Permission
		ok    bool
	}{
		{"local anonymous reads", local, identity.OperatorID, chat.PermRead, true},
		{"local anonymous controls", local, identity.OperatorID, chat.PermControl, true},
		{"unverified claim is anonymous", claimed, identity.OperatorID, chat.PermTakeover, true},
		{"operator takes over", operator, identity.OperatorID, chat.PermTakeover, true},
		{"read device reads", reader, identity.OperatorID, chat.PermRead, true},
		{"read device cannot control", reader, identity.OperatorID, chat.PermControl, false},
		{"operate device controls", operate, identity.OperatorID, chat.PermControl, true},
		{"operate device takes over", operate, identity.OperatorID, chat.PermTakeover, true},
		{"admin is refused", operator, identity.OperatorID, chat.PermAdmin, false},
		{"another owner is refused", operator, "msg://user/someone/else", chat.PermRead, false},
	}
	for _, c := range cases {
		err := Authorize(c.ctx, c.owner, c.perm)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrForbidden)) {
			t.Errorf("%s: Authorize = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}
