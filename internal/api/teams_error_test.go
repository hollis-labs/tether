package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

func TestTeamErrorForCarriesCauseOnlyToLocalOperator(t *testing.T) {
	cause := errors.New("launch \"x\" not found\n" + strings.Repeat("y", 1000))
	err := &teamsvc.Error{Code: "operation_failed", Cause: cause}
	local := identity.ConnectionContext(context.Background(), &net.UnixConn{})
	remote := identity.ConnectionContext(context.Background(), &net.TCPConn{})
	op := identity.Principal{ID: identity.OperatorID, Kind: "operator", Verified: true}

	status, detail := TeamErrorFor(identity.WithPrincipal(local, op), err)
	if status != 500 || detail.Code != CodeInternalError {
		t.Fatalf("status/code = %d/%s", status, detail.Code)
	}
	if !strings.HasPrefix(detail.Message, "internal_error: launch \"x\" not found y") || strings.Contains(detail.Message, "\n") || len(detail.Message) > 400 {
		t.Fatalf("operator message = %q", detail.Message)
	}
	for name, ctx := range map[string]context.Context{
		"remote operator": identity.WithPrincipal(remote, op),
		"service":         identity.WithPrincipal(local, identity.Principal{ID: "msg://service/local/m", Kind: "service", Verified: true}),
		"anonymous":       local,
	} {
		if _, d := TeamErrorFor(ctx, err); d.Message != "internal_error" {
			t.Fatalf("%s saw cause: %q", name, d.Message)
		}
	}
	// Classified failures carry their cause for the operator only.
	classified := fmt.Errorf("%w: binding held", teamsvc.ErrUnavailable)
	if _, d := TeamErrorFor(identity.WithPrincipal(local, op), classified); d.Code != "unavailable" || !strings.HasPrefix(d.Message, "unavailable: ") || !strings.Contains(d.Message, "binding held") {
		t.Fatalf("operator unavailable message = %q", d.Message)
	}
	if _, d := TeamErrorFor(local, classified); d.Message != "unavailable" {
		t.Fatalf("anonymous saw cause: %q", d.Message)
	}
}
