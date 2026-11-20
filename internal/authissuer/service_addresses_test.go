package authissuer

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

type serviceAddressRegistry map[string]registry.Profile

func (r serviceAddressRegistry) Lookup(_ context.Context, urn string) (registry.Profile, error) {
	p, ok := r[urn]
	if !ok {
		return p, errors.New("not found")
	}
	return p, nil
}

func TestServiceAddressBindingsPersistAndReissue(t *testing.T) {
	is, dir := newIssuer(t)
	const a = "msg://agent/agent-mux/agt_first"
	const b = "msg://agent/agent-mux/agt_second"
	is.ServiceAddresses = serviceAddressRegistry{a: {URN: a, Kind: registry.KindAgent, Status: registry.StatusActive}, b: {URN: b, Kind: registry.KindAgent, Status: registry.StatusActive}}
	req := ServiceRequest{App: "surface", Audience: "tether", Scopes: []string{"messages", "read"}, Addresses: []string{a, b, a}}
	tok, rec, err := is.IssueService(operatorP, req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{a, b}
	if !reflect.DeepEqual(rec.Addresses, want) || !reflect.DeepEqual(claimsOf(t, tok.Token).Addresses, want) {
		t.Fatal("ordered deduplicated bindings missing")
	}
	// The returned metadata and list snapshots cannot mutate durable grants.
	rec.Addresses[0] = b
	req.Addresses[0] = b
	rows := is.State.Services()
	rows[0].Addresses[0] = b
	if !reflect.DeepEqual(is.State.Services()[0].Addresses, want) {
		t.Fatal("mutable metadata changed durable bindings")
	}
	restored, err := OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	is.State = restored
	if !reflect.DeepEqual(restored.Services()[0].Addresses, want) {
		t.Fatal("bindings lost across restart")
	}
	rotation, err := is.RotateServiceKey(operatorP, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rotation.Reissued) != 1 || !reflect.DeepEqual(rotation.Reissued[0].Record.Addresses, want) {
		t.Fatal("rotation lost grants")
	}
	fresh, err := NewVerifier(nil, is)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{tok.Token, rotation.Reissued[0].Token.Token} {
		p, err := fresh.Verify(t.Context(), token)
		if err != nil || !p.MayActAs(a) || !p.MayActAs(b) || p.MayActAs("msg://agent/agent-mux/agt_other") {
			t.Fatal("signed grants widened or lost", err)
		}
	}
}

func TestServiceAddressRegistryRequiredAndBounded(t *testing.T) {
	is, _ := newIssuer(t)
	const a = "msg://agent/agent-mux/agt_first"
	req := ServiceRequest{App: "surface", Audience: "tether", Scopes: []string{"messages"}, Addresses: []string{a}}
	if _, _, err := is.IssueService(operatorP, req); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil registry accepted address")
	}
	is.ServiceAddresses = serviceAddressRegistry{a: {URN: a, Kind: registry.KindAgent, Status: registry.StatusActive, MergedInto: "msg://agent/agent-mux/agt_other"}}
	if _, _, err := is.IssueService(operatorP, req); !errors.Is(err, ErrInvalid) {
		t.Fatal("merged address accepted")
	}
	req.Addresses = make([]string, 17)
	if _, _, err := is.IssueService(operatorP, req); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbounded addresses accepted")
	}
	if len(is.State.Services()) != 0 {
		t.Fatal("invalid binding left service metadata")
	}
}
