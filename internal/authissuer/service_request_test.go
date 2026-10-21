package authissuer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

func sessionCaller(app string) SessionCaller {
	return SessionCaller{PrincipalID: SessionPrincipalID("s1"), SessionID: "s1", App: app}
}

func TestRequestServiceIssuesOnlyTheSessionsOwnApp(t *testing.T) {
	is, _ := newIssuer(t)
	tok, rec, err := is.RequestService(sessionCaller("torque"), SessionServiceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Sub != "msg://service/torque/dev" || rec.App != "torque" || rec.Audience != TetherAudience {
		t.Fatalf("record %+v", rec)
	}
	if len(rec.Scopes) != 1 || rec.Scopes[0] != "tether.events" {
		t.Fatalf("default scope %v", rec.Scopes)
	}
	if rec.RequestedBy != SessionPrincipalID("s1") || rec.RequestedSession != "s1" || rec.IssuedBy != rec.RequestedBy {
		t.Fatalf("requester not recorded: %+v", rec)
	}
	// It verifies like an operator-issued service token: same kind, key family, record.
	v, err := NewVerifier(fakeStored{map[string]identity.Principal{}}, is)
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Verify(context.Background(), tok.Token)
	if err != nil || p.ID != "msg://service/torque/dev" || !is.LiveService(rec.JTI) {
		t.Fatalf("verify: %+v %v live=%v", p, err, is.LiveService(rec.JTI))
	}
	// Revoking by app reaches requested tokens too.
	if _, err := is.RevokeAllServices("torque"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), tok.Token); err == nil {
		t.Fatal("a requested token survived revoke --all --app")
	}
}

func TestRequestServiceRefusals(t *testing.T) {
	is, _ := newIssuer(t)
	ended := sessionCaller("torque")
	ended.Ended = true
	noSession := sessionCaller("torque")
	noSession.SessionID = ""
	for name, c := range map[string]SessionCaller{
		"no app (NULL)": sessionCaller(""),
		"ended session": ended,
		"no session id": noSession,
		"malformed app": sessionCaller("Torque/../x"),
		"app too long":  sessionCaller(strings.Repeat("a", 40)),
	} {
		if tok, _, err := is.RequestService(c, SessionServiceRequest{}); err == nil || tok.Token != "" {
			t.Errorf("%s: minted or no error: %v", name, err)
		}
	}
	if n := len(is.State.Services()); n != 0 {
		t.Fatalf("%d records written by refused requests", n)
	}
}

func TestRequestServiceScopeAllowlist(t *testing.T) {
	is, _ := newIssuer(t)
	for _, scopes := range [][]string{
		{"tether.read"}, {"tether.messages"}, {"tether.events", "tether.read"},
		{"tether.*"}, {"torque.approve"}, {"*"}, {"events", "bogus"},
	} {
		if _, _, err := is.RequestService(sessionCaller("torque"), SessionServiceRequest{Scopes: scopes}); err == nil {
			t.Errorf("scopes %v were accepted", scopes)
		}
	}
	if _, _, err := is.RequestService(sessionCaller("torque"), SessionServiceRequest{Scopes: []string{"events"}}); err != nil {
		t.Fatalf("short scope name events: %v", err)
	}
	if _, _, err := is.RequestService(sessionCaller("torque"), SessionServiceRequest{Label: "bad label (parens)"}); err == nil {
		t.Fatal("bad label accepted")
	}
}

func TestRequestServiceCapPerSessionAndApp(t *testing.T) {
	is, _ := newIssuer(t)
	var jtis []string
	for i := 0; i < maxRequestedPerSession; i++ {
		_, rec, err := is.RequestService(sessionCaller("torque"), SessionServiceRequest{})
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		jtis = append(jtis, rec.JTI)
	}
	if _, _, err := is.RequestService(sessionCaller("torque"), SessionServiceRequest{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cap not enforced: %v", err)
	}
	// Another session of the same app has its own allowance; the operator's own tokens do not count.
	other := SessionCaller{PrincipalID: SessionPrincipalID("s2"), SessionID: "s2", App: "torque"}
	if _, _, err := is.RequestService(other, SessionServiceRequest{}); err != nil {
		t.Fatalf("another session: %v", err)
	}
	// A revoked token frees a slot.
	if _, err := is.RevokeService(jtis[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := is.RequestService(sessionCaller("torque"), SessionServiceRequest{}); err != nil {
		t.Fatalf("after revoke: %v", err)
	}
}

func TestRequestServiceCapIsAtomicUnderConcurrency(t *testing.T) {
	is, _ := newIssuer(t)
	const n = 40
	res := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, _, err := is.RequestService(sessionCaller("torque"), SessionServiceRequest{})
			res <- err
		}()
	}
	ok := 0
	for i := 0; i < n; i++ {
		if <-res == nil {
			ok++
		}
	}
	if ok != maxRequestedPerSession {
		t.Fatalf("%d concurrent requests succeeded, want exactly %d", ok, maxRequestedPerSession)
	}
}

func TestRequestServiceCannotBeReachedWithAnOperatorPrincipalAndExchangeStillRefusesSessions(t *testing.T) {
	is, _ := newIssuer(t)
	// RequestService takes a SessionCaller built by the hub from a session row, not a
	// Principal: an operator has no session id, so there is nothing to hand it.
	if _, _, err := is.RequestService(SessionCaller{PrincipalID: identity.OperatorID, App: "torque"}, SessionServiceRequest{}); err == nil {
		t.Fatal("an operator principal without a session minted through RequestService")
	}
	// The exchange stays closed to agent and session parents for every audience.
	for _, p := range []identity.Principal{
		{Verified: true, ID: SessionPrincipalID("s1"), Kind: "session", SessionID: "s1", Scopes: []string{"*"}},
		{Verified: true, ID: SessionPrincipalID("s1"), Kind: "agent", SessionID: "s1", TokenID: "j", Scopes: []string{"tether.events"}},
	} {
		for _, aud := range []string{TetherAudience, "torque"} {
			_, err := is.Exchange(p, Request{Audience: aud, Scopes: []string{aud + ".events"}})
			if err == nil {
				t.Errorf("%s parent minted for %s through Exchange", p.Kind, aud)
			}
		}
	}
}
