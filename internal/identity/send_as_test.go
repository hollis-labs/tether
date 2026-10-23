package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestPrincipalMayActAsAndDefaultAddress(t *testing.T) {
	session := identity.Principal{ID: "msg://session/local/s1", Kind: "session", Addresses: []string{"msg://session/local/s1", "msg://agent/local/worker"}}
	device := identity.Principal{ID: "msg://device/d1", Kind: "device", Scopes: []string{"operate"}}
	grant := identity.Principal{ID: "msg://session/local/s2", Scopes: []string{identity.ScopeSendAs}}
	operator := identity.Principal{ID: identity.OperatorID, Scopes: []string{"*"}}
	for _, tc := range []struct {
		name string
		p    identity.Principal
		addr string
		want bool
	}{
		{"own id", session, "msg://session/local/s1", true},
		{"listed address", session, "msg://agent/local/worker", true},
		{"other address", session, "msg://agent/local/other", false},
		{"empty", session, "", false},
		{"device id", device, "msg://device/d1", true},
		{"device derived sender", device, "msg://service/device/d1", true},
		{"device other", device, "msg://service/device/d2", false},
		{"send_as grant", grant, "msg://agent/local/anyone", true},
		{"operator wildcard", operator, "msg://agent/local/anyone", true},
	} {
		if got := tc.p.MayActAs(tc.addr); got != tc.want {
			t.Errorf("%s: MayActAs(%q) = %v, want %v", tc.name, tc.addr, got, tc.want)
		}
	}
	if got := session.DefaultAddress(); got != "msg://session/local/s1" {
		t.Errorf("session default = %q", got)
	}
	if got := device.DefaultAddress(); got != "msg://service/device/d1" {
		t.Errorf("device default = %q", got)
	}
}

func TestRecordObservationPersistsEvent(t *testing.T) {
	s, db := identityStore(t)
	err := s.RecordObservation(context.Background(), identity.Observation{
		At: time.Now(), PrincipalID: "msg://session/local/s1", Mode: identity.Observe,
		Authentication: "verified", Method: "POST", Route: "/messages", Event: "from_unverified"})
	if err != nil {
		t.Fatal(err)
	}
	var event string
	if err := db.DB().QueryRow(`SELECT event FROM identity_audit WHERE route='/messages'`).Scan(&event); err != nil || event != "from_unverified" {
		t.Fatalf("event = %q err=%v", event, err)
	}
}

// CW-20261023-0018 (Architect condition d): MayActAs is an exact-string rule, so the
// four-segment agent instance form is neither granted nor denied by accident. A grant on the
// agent's address or on the session's own address does not cover it, an explicit entry does,
// and message.send_as keeps meaning "any address" as before.
func TestMayActAsAgentInstanceAddress(t *testing.T) {
	const agent = "msg://agent/agent-mux/worker"
	const instance = agent + "/s1"
	for _, tc := range []struct {
		name string
		p    Principal
		addr string
		want bool
	}{
		{"agent address does not cover its instance", Principal{ID: "msg://device/d", Addresses: []string{agent}}, instance, false},
		{"session address does not cover the instance", Principal{ID: "msg://session/local/s1", SessionID: "s1", Addresses: []string{"msg://session/local/s1"}}, instance, false},
		{"instance entry does not cover the agent address", Principal{ID: "msg://device/d", Addresses: []string{instance}}, agent, false},
		{"another agent's instance with the same id", Principal{ID: "msg://device/d", Addresses: []string{instance}}, "msg://agent/agent-mux/general/s1", false},
		{"explicit entry is honoured as an exact string", Principal{ID: "msg://device/d", Addresses: []string{instance}}, instance, true},
		{"a prefix of the agent id is not the agent", Principal{ID: "msg://device/d", Addresses: []string{"msg://agent/agent-mux/work"}}, instance, false},
		{"send_as still covers any address", Principal{ID: "msg://device/d", Scopes: []string{ScopeSendAs}}, instance, true},
	} {
		if got := tc.p.MayActAs(tc.addr); got != tc.want {
			t.Errorf("%s: MayActAs(%q) = %v, want %v", tc.name, tc.addr, got, tc.want)
		}
	}
}
