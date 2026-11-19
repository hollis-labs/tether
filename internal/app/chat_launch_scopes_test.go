package app

import (
	"context"
	"slices"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// pairedDevice is a chat caller the operator paired (Tachyon), holding scopes.
func pairedDevice(scopes ...string) context.Context {
	return identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "msg://device/tachyon", Kind: "device", CreatedBy: identity.OperatorID, Scopes: scopes})
}

// CW-20261114-0063: a Tachyon chat launch of a catalog seat is bounded by the
// seat's catalog definition, not by the device that asked; every other launch
// is still bounded by its launcher and says so with session.scopes_narrowed.
func TestChatLaunchOfACatalogSeatGetsItsCatalogScopes(t *testing.T) {
	defaults := launchprofile.DefaultMCPScopes
	if len(defaults) < 2 {
		t.Fatalf("test needs at least two default scopes, have %v", defaults)
	}
	one := defaults[0]
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	r.withBus()
	r.svc.Catalog = &config.Catalog{Agents: map[string]config.LaunchProfile{"portfolio-manager": {}}}

	// A chat launch by the operator's paired device holding one scope gets the seat's full set.
	spec, err := r.launchAs(withChatLaunch(pairedDevice(one)), "sess-chat", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.MCPScopes, defaults) || !slices.Equal(r.storedScopes("sess-chat"), defaults) {
		t.Fatalf("chat launch: spec %v, credential %v; want %v", spec.MCPScopes, r.storedScopes("sess-chat"), defaults)
	}
	if a := r.attribution("sess-chat"); a != string(AttributionCatalog) {
		t.Fatalf("attribution %q, want %q", a, AttributionCatalog)
	}
	if ev := r.narrowedEvents("sess-chat"); len(ev) != 0 {
		t.Fatalf("unexpected narrowing on a catalog chat launch: %v", ev)
	}

	// The same device launching the same seat other than by chat is still bounded by it.
	if _, err = r.launchAs(pairedDevice(one), "sess-api", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.storedScopes("sess-api"); !slices.Equal(got, []string{one}) {
		t.Fatalf("non-chat launch by a narrow launcher: %v", got)
	}
	if ev := r.narrowedEvents("sess-api"); len(ev) != 1 || ev[0]["basis"] != ScopeBasisLauncher {
		t.Fatalf("narrowed events = %v", ev)
	}

	// A chat launch of a seat whose earlier session was launched narrow still
	// gets the catalog's scopes: the catalog bound is checked before the parent grant.
	resume := withChatLaunch(withLaunchOrigin(pairedDevice(one), InternalLaunch{Kind: OriginResume, OriginSessionID: "sess-api"}))
	if _, err = r.launchAs(resume, "sess-chat-resume", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.storedScopes("sess-chat-resume"); !slices.Equal(got, defaults) {
		t.Fatalf("chat relaunch of a narrow seat: %v, want %v", got, defaults)
	}

	// A chat launch with no principal (identity observe) gets no catalog basis:
	// it stays anonymous_external, which the enforce-flip guard counts.
	if _, err = r.launchAs(withChatLaunch(context.Background()), "sess-anon", ""); err != nil {
		t.Fatal(err)
	}
	if a := r.attribution("sess-anon"); a != string(AttributionAnonymous) {
		t.Fatalf("anonymous chat launch attribution %q, want %q", a, AttributionAnonymous)
	}

	// A chat launch of a seat the catalog does not define is bounded by the launcher.
	r.svc.Catalog = &config.Catalog{Agents: map[string]config.LaunchProfile{}}
	if _, err = r.launchAs(withChatLaunch(pairedDevice(one)), "sess-chat-adhoc", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.storedScopes("sess-chat-adhoc"); !slices.Equal(got, []string{one}) {
		t.Fatalf("chat launch of a non-catalog seat: %v", got)
	}
	if ev := r.narrowedEvents("sess-chat-adhoc"); len(ev) != 1 || ev[0]["basis"] != ScopeBasisLauncher {
		t.Fatalf("narrowed events = %v", ev)
	}
}

// CW-20261114-0063 review: only the operator gets the catalog basis on a chat
// launch, so a holder of operate cannot widen what it holds by launching a
// seat (e.g. chief, whose catalog grants admin). Every other caller is bounded
// by itself and session.scopes_narrowed fires.
func TestChatLaunchCatalogBasisIsOperatorOnly(t *testing.T) {
	defaults := launchprofile.DefaultMCPScopes
	one := defaults[0]
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	r.withBus()
	r.svc.Catalog = &config.Catalog{Agents: map[string]config.LaunchProfile{"portfolio-manager": {}}}

	operator := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{one}})
	if _, err := r.launchAs(withChatLaunch(operator), "sess-operator", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.storedScopes("sess-operator"); !slices.Equal(got, defaults) || r.attribution("sess-operator") != string(AttributionCatalog) {
		t.Fatalf("operator chat launch: %v (%s), want %v (catalog)", got, r.attribution("sess-operator"), defaults)
	}

	callers := map[string]identity.Principal{
		// An operate token minted from the paired device.
		"sess-token": {Verified: true, ID: "msg://device/tachyon", Kind: "device", TokenID: "tok_operate", CreatedBy: identity.OperatorID, Scopes: []string{one}},
		// A device someone other than the operator paired.
		"sess-other-device": {Verified: true, ID: "msg://device/elsewhere", Kind: "device", CreatedBy: "msg://agent/test/pairer", Scopes: []string{one}},
		// An agent holding operate.
		"sess-agent": {Verified: true, ID: "msg://agent/test/launcher", Kind: "agent", Scopes: []string{one}},
	}
	for id, p := range callers {
		if _, err := r.launchAs(withChatLaunch(identity.WithPrincipal(context.Background(), p)), id, ""); err != nil {
			t.Fatal(err)
		}
		if got := r.storedScopes(id); !slices.Equal(got, []string{one}) {
			t.Errorf("%s: chat launch widened to %v", id, got)
		}
		if a := r.attribution(id); a == string(AttributionCatalog) {
			t.Errorf("%s: attribution %q", id, a)
		}
		if ev := r.narrowedEvents(id); len(ev) != 1 || ev[0]["basis"] != ScopeBasisLauncher {
			t.Errorf("%s: narrowed events = %v", id, ev)
		}
	}
}
