package teamruntime

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

type ceilings struct{}

func (ceilings) FormationCeilings(context.Context, teamsvc.Principal) (teamsvc.FormationPolicy, error) {
	return teamsvc.ConservativePolicy(), nil
}

type unusedMessages struct{}

func (unusedMessages) QueueTeamDelivery(context.Context, teams.Delivery) error {
	return errors.New("unexpected delivery")
}
func serviceFixture(t *testing.T, resolver teamsvc.Principals) *teamsvc.Service {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	sessions, err := NewSessions(db, s, &sessionEffects{db: db}, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	host, err := teamhost.New(db.DB(), s, teamhost.Ports{Enroller: e, Sessions: sessions, Messenger: messages, Channels: Channels{}}, teamhost.Options{})
	check(t, err)
	service, err := teamsvc.New(teamsvc.Deps{Ceilings: ceilings{}, Principals: resolver, Runs: host, Calls: host, Definitions: s, Roster: s, Ledger: s, Signals: s, Provisioner: host, Workflows: host, Triggers: host, Sender: host, Routing: host, Trust: host, Approvals: host, Clock: host, IDs: host, Defaults: teamsvc.ConservativePolicy().Limits})
	check(t, err)
	return service
}
func TestPrincipalsRefuseAssertionsObserveAndAcceptVerifiedThroughService(t *testing.T) {
	db, _, _ := dbFixture(t, filepath.Join(t.TempDir(), "identity"))
	auth := identity.NewStore(db.DB())
	token, err := auth.Mint(ctx, identity.Principal{ID: "msg://service/local/verified", Kind: "service", Display: "Verified"})
	check(t, err)
	for _, tc := range []struct {
		name   string
		mode   identity.Mode
		bearer string
		want   error
	}{
		{"asserted header and query", identity.Enforce, "", teamsvc.ErrUnauthenticated},
		{"invalid bearer in observe", identity.Observe, "asserted-token", teamsvc.ErrUnauthenticated},
		{"verified attribution in observe", identity.Observe, token, teamsvc.ErrUnauthenticated},
		{"verified credential", identity.Enforce, token, teamsvc.ErrInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := serviceFixture(t, Principals{Mode: tc.mode})
			var result error
			// Observe middleware lets absent/invalid identity reach the service; the
			// independently configured adapter must still refuse it.
			middleware := identity.Middleware(identity.Observe, auth, nil, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				_, result = service.Form(r.Context(), teamsvc.FormRequest{Key: "key"})
			}))
			request := httptest.NewRequest(http.MethodPost, "/teams/form?as=msg://user/local/mallory", nil)
			request.Header.Set("X-Principal", "msg://user/local/mallory")
			request.Header.Set("X-Local-Operator", "true")
			if tc.bearer != "" {
				request.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			middleware.ServeHTTP(httptest.NewRecorder(), request)
			var typed *teamsvc.Error
			if !errors.Is(result, tc.want) || !errors.As(result, &typed) {
				t.Fatalf("result=%v want typed %v", result, tc.want)
			}
		})
	}
}
func TestLocalOperatorNeedsVerifiedCredentialAndAcceptedUnixSocket(t *testing.T) {
	principal := identity.Principal{ID: identity.OperatorID, Kind: "operator"}
	resolver := Principals{Mode: identity.Enforce}
	verified := identity.WithPrincipal(ctx, principal)
	if _, err := resolver.ResolvePrincipal(verified); !errors.Is(err, teamsvc.ErrUnauthenticated) {
		t.Fatal("nonlocal operator accepted", err)
	}
	socketRoot, err := os.MkdirTemp("/var/tmp", "team-principal.")
	check(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	listener, err := net.Listen("unix", filepath.Join(socketRoot, "socket"))
	check(t, err)
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() { connection, _ := listener.Accept(); accepted <- connection }()
	client, err := net.Dial("unix", listener.Addr().String())
	check(t, err)
	defer client.Close()
	connection := <-accepted
	defer connection.Close()
	local := identity.ConnectionContext(ctx, connection)
	wrong := identity.WithPrincipal(local, identity.Principal{ID: "msg://user/local/mallory", Kind: "operator"})
	if _, err = resolver.ResolvePrincipal(wrong); !errors.Is(err, teamsvc.ErrUnauthenticated) {
		t.Fatal("operator kind with arbitrary ID accepted", err)
	}
	tcpListener, tcpErr := net.Listen("tcp", "127.0.0.1:0")
	check(t, tcpErr)
	defer tcpListener.Close()
	tcpAccepted := make(chan net.Conn, 1)
	go func() { c, _ := tcpListener.Accept(); tcpAccepted <- c }()
	tcpClient, tcpErr := net.Dial("tcp", tcpListener.Addr().String())
	check(t, tcpErr)
	defer tcpClient.Close()
	tcpConn := <-tcpAccepted
	defer tcpConn.Close()
	if _, err = resolver.ResolvePrincipal(identity.WithPrincipal(identity.ConnectionContext(ctx, tcpConn), principal)); !errors.Is(err, teamsvc.ErrUnauthenticated) {
		t.Fatal("TCP operator accepted", err)
	}
	if _, err = resolver.ResolvePrincipal(local); !errors.Is(err, teamsvc.ErrUnauthenticated) {
		t.Fatal("socket alone granted operator", err)
	}
	db, _, _ := dbFixture(t, filepath.Join(t.TempDir(), "operator-db"))
	auth := identity.NewStore(db.DB())
	token, err := auth.Mint(ctx, principal)
	check(t, err)
	var out teamsvc.Principal
	var result error
	middleware := identity.Middleware(identity.Enforce, auth, nil, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { out, result = resolver.ResolvePrincipal(r.Context()) }))
	request := httptest.NewRequest(http.MethodPost, "/teams/form", nil).WithContext(local)
	request.Header.Set("Authorization", "Bearer "+token)
	middleware.ServeHTTP(httptest.NewRecorder(), request)
	check(t, result)
	if !out.LocalOperator || !out.Verified || out.ID != teamsvc.LocalOperator {
		t.Fatal("authenticated local operator absent", out)
	}
	spoof := identity.WithPrincipal(local, identity.Principal{ID: identity.OperatorID, Kind: "interactive"})
	if out, err = resolver.ResolvePrincipal(spoof); err == nil && out.LocalOperator {
		t.Fatal("reserved URN gained operator by name")
	}
}
func TestVerifiedSessionUsesOnlyItsRetainedActorAndBinding(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	port, e, _, req := sessionFixture(t, db, s, reg, "principal")
	id, err := port.Launch(ctx, req)
	check(t, err)
	resolver := Principals{Mode: identity.Enforce, Sessions: e}
	principal := identity.Principal{ID: "msg://session/local/" + id, Kind: "session", SessionID: id}
	out, err := resolver.ResolvePrincipal(identity.WithPrincipal(ctx, principal))
	check(t, err)
	if out.ID != req.Actor || !out.Verified {
		t.Fatal("verified session misattributed", out)
	}
	principal.SessionID = "another"
	if _, err = resolver.ResolvePrincipal(identity.WithPrincipal(ctx, principal)); !errors.Is(err, teamsvc.ErrUnauthenticated) {
		t.Fatal("session selector trusted", err)
	}
}
