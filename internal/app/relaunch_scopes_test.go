package app

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func (r *identityRig) withBus() {
	if r.svc.Bus == nil {
		r.svc.Bus = events.NewBus(events.BusOptions{Persister: r.db})
	}
}

func (r *identityRig) narrowedEvents(sessionID string) []map[string]any {
	r.t.Helper()
	rows, err := r.db.DB().Query(`SELECT payload_json FROM events WHERE kind=? AND session_id=?`, KindSessionScopesNarrowed, sessionID)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		_ = rows.Scan(&raw)
		var p map[string]any
		_ = json.Unmarshal([]byte(raw), &p)
		out = append(out, p)
	}
	return out
}

func (r *identityRig) attribution(id string) string {
	r.t.Helper()
	var a string
	if err := r.db.DB().QueryRow(`SELECT coalesce(launch_attribution,'') FROM principals WHERE kind='session' AND session_id=? AND revoked_at IS NULL`, id).Scan(&a); err != nil {
		r.t.Fatal(err)
	}
	return a
}

// CW-20261114-0038: a resume of the same seat keeps what the seat's previous
// session held, whoever triggers it; a first launch is still bounded by the
// launcher, and a narrowing publishes session.scopes_narrowed.
func TestRelaunchOfTheSameSeatKeepsItsGrant(t *testing.T) {
	defaults := launchprofile.DefaultMCPScopes
	if len(defaults) < 2 {
		t.Fatalf("test needs at least two default scopes, have %v", defaults)
	}
	one := defaults[0]
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	r.withBus()

	// The seat's first session, launched by a principal holding everything.
	if _, err := r.launchAs(launcherWith("*"), "sess-parent", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.storedScopes("sess-parent"); !slices.Equal(got, defaults) {
		t.Fatalf("parent scopes %v", got)
	}

	// A first launch by a narrow launcher is narrowed, and says so.
	if _, err := r.launchAs(launcherWith(one), "sess-fresh", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.storedScopes("sess-fresh"); !slices.Equal(got, []string{one}) {
		t.Fatalf("fresh launch by a narrow launcher: %v", got)
	}
	ev := r.narrowedEvents("sess-fresh")
	if len(ev) != 1 || ev[0]["basis"] != ScopeBasisLauncher || len(ev[0]["dropped"].([]any)) != len(defaults)-1 {
		t.Fatalf("narrowed events = %v", ev)
	}

	// The same launcher resuming the same seat keeps the parent's grant.
	resume := withLaunchOrigin(launcherWith(one), InternalLaunch{Kind: OriginResume, OriginSessionID: "sess-parent"})
	spec, err := r.launchAs(resume, "sess-resumed", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.MCPScopes, defaults) || !slices.Equal(r.storedScopes("sess-resumed"), defaults) {
		t.Fatalf("resume of the same seat: spec %v, credential %v; want %v", spec.MCPScopes, r.storedScopes("sess-resumed"), defaults)
	}
	if a := r.attribution("sess-resumed"); a != string(AttributionOrigin) {
		t.Fatalf("attribution %q, want %q (the parent grant vouched)", a, AttributionOrigin)
	}
	if ev := r.narrowedEvents("sess-resumed"); len(ev) != 0 {
		t.Fatalf("unexpected narrowing on the same-seat resume: %v", ev)
	}

	// Nobody gains what the seat never held: resuming a narrowed session keeps it narrow.
	resumeFresh := withLaunchOrigin(launcherWith("*"), InternalLaunch{Kind: OriginResume, OriginSessionID: "sess-fresh"})
	if _, err := r.launchAs(resumeFresh, "sess-from-fresh", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.storedScopes("sess-from-fresh"); !slices.Equal(got, []string{one}) {
		t.Fatalf("resume of a narrow session widened to %v", got)
	}

	// A resume naming another seat's session gets no grant from it: the launcher bounds it.
	if _, err := r.db.DB().Exec(`UPDATE sessions SET logical_agent_id='someone-else' WHERE id='sess-parent'`); err != nil {
		t.Fatal(err)
	}
	other := withLaunchOrigin(launcherWith(one), InternalLaunch{Kind: OriginResume, OriginSessionID: "sess-parent"})
	if _, err := r.launchAs(other, "sess-cross", ""); err != nil {
		t.Fatal(err)
	}
	if got := r.storedScopes("sess-cross"); !slices.Equal(got, []string{one}) {
		t.Fatalf("cross-seat resume took the other seat's grant: %v", got)
	}
}
