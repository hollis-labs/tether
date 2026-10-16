package teamhost_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamstore"
)

const (
	operator    mesh.URN = "msg://user/local/operator"
	fleetActor  mesh.URN = "msg://agent/fleet/one"
	fleetActor2 mesh.URN = "msg://agent/fleet/two"
)

var adoptPin = mesh.DefinitionRef{ID: "worker", Revision: "r1", Digest: "sha256:aaaa"}

// fakeAdopter models a live fleet: sessions that already run, bound by the
// registry under a generation, which the team host must never stop.
type fakeAdopter struct {
	mu       sync.Mutex
	live     map[mesh.URN]teamhost.Adoption
	released map[string]int
	releases []string
	verifyN  int
}

func newAdopter() *fakeAdopter {
	a := &fakeAdopter{live: map[mesh.URN]teamhost.Adoption{}, released: map[string]int{}}
	for i, actor := range []mesh.URN{fleetActor, fleetActor2} {
		a.live[actor] = teamhost.Adoption{Actor: actor, AgentID: string(actor), Kind: mesh.ActorAgent, SessionID: []string{"fleet-session-1", "fleet-session-2"}[i], Generation: 3, Resolved: true, Owner: operator, LaunchID: "tether-claude", Pin: adoptPin}
	}
	return a
}
func (a *fakeAdopter) Verify(_ context.Context, c teamhost.AdoptionCheck) (teamhost.Adoption, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.verifyN++
	v, ok := a.live[c.Actor]
	if !ok {
		return teamhost.Adoption{}, teams.ErrNotFound
	}
	return v, nil
}
func (a *fakeAdopter) Release(_ context.Context, key string, m teams.Member) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.released[key]++
	a.releases = append(a.releases, m.SessionID)
	return nil
}
func (a *fakeAdopter) mutate(actor mesh.URN, fn func(*teamhost.Adoption)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := a.live[actor]
	fn(&v)
	a.live[actor] = v
}

func adoptDefinition() teams.Team {
	d := definition()
	for i := range d.Slots {
		if d.Slots[i].Name == "worker" {
			d.Slots[i].Definition = adoptPin
			d.Slots[i].Resolution = teams.Pool
			d.Slots[i].Pool = "fleet"
			d.Slots[i].Identities = []mesh.URN{fleetActor, fleetActor2}
			d.Slots[i].Activation = teams.Concurrent
			d.Slots[i].Max = 2
		}
	}
	d.Authority.Grants = append(d.Authority.Grants, teams.Grant{FromSlot: "root", Verb: teams.MaySpawn, ToSlot: "subworker"})
	return d
}
func adoptOptions() teamhost.Options {
	o := options()
	o.TrustTiers[adoptPin] = teamhost.TrustTrusted
	o.AdoptableLaunches = teamhost.AdoptedLaunches{"tether-claude": adoptPin}
	return o
}
func openAdopt(t *testing.T, path string, f *fakePorts, a teamhost.Adopter, o teamhost.Options) (*store.Store, *teamstore.Store, *teamhost.Host) {
	t.Helper()
	db, err := store.Open(path)
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	storage, err := teamstore.New(db.DB(), teamstore.Options{})
	must(t, err)
	host, err := teamhost.New(db.DB(), storage, teamhost.Ports{Sessions: f, Enroller: f, Messenger: f, Channels: f, Adopter: a}, o)
	must(t, err)
	return db, storage, host
}
func adoptRun(t *testing.T, s *teamstore.Store, h *teamhost.Host) teams.TeamRun {
	t.Helper()
	must(t, s.PutDefinition(ctx, adoptDefinition()))
	run, err := launcher(s, h).Launch(ctx, teams.LaunchRequest{Key: "launch", TeamID: "team-one", Version: 1})
	must(t, err)
	return run
}
func adoptRequest(run teams.TeamRun, key string, actor mesh.URN, session string) teamhost.AdoptRequest {
	return teamhost.AdoptRequest{
		Provision:         teams.ProvisionRequest{IdempotencyKey: key, RunID: run.ID, MemberID: "m-" + key, Slot: teams.Slot{Name: "worker", Definition: adoptPin}, Identity: actor, Limits: childLimits()},
		SessionID:         session,
		BindingGeneration: 3,
		Principal:         operator,
	}
}

func TestAdoptedMemberIsReleasedNotStopped(t *testing.T) {
	for _, mode := range []string{"release", "retire"} {
		t.Run(mode, func(t *testing.T) {
			f, a := fake(), newAdopter()
			_, s, h := openAdopt(t, t.TempDir()+"/state.db", f, a, adoptOptions())
			run := adoptRun(t, s, h)
			member, err := h.Adopt(ctx, adoptRequest(run, "adopt-1", fleetActor, "fleet-session-1"))
			must(t, err)
			if member.SessionID != "fleet-session-1" || member.SpawnCapable || member.Governance != teams.MemberRole || member.Resolution != teams.Durable {
				t.Fatalf("adopted member: %+v", member)
			}
			f.mu.Lock()
			launched := len(f.launches)
			f.mu.Unlock()
			if mode == "release" {
				must(t, h.Release(ctx, run.ID, member))
			} else if err = h.Retire(ctx, run.ID, member); !errors.Is(err, teams.ErrConflict) {
				// Adoption is never ephemeral, so a retire request is a conflict.
				t.Fatalf("retire of an adopted member: %v", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if mode == "release" {
				if f.stopped["adopt-1"] || len(f.stopOrder) != 0 {
					t.Fatalf("adopted session was stopped: %v", f.stopOrder)
				}
				if f.bindingEnded["adopt-1"] || f.enrollmentEnded["adopt-1"] {
					t.Fatal("registry binding or enrollment was released by the team host")
				}
				if a.released["adopt-1"] != 1 || len(a.releases) != 1 || a.releases[0] != "fleet-session-1" {
					t.Fatalf("adopter release: %v %v", a.released, a.releases)
				}
			}
			if len(f.launches) != launched {
				t.Fatal("adoption launched a session")
			}
		})
	}
}

// The provisioned half of the same run is still stopped: only adoption is exempt.
func TestDissolveStopsProvisionedAndKeepsAdoptedSessionAliveAndBound(t *testing.T) {
	f, a := fake(), newAdopter()
	_, s, h := openAdopt(t, t.TempDir()+"/state.db", f, a, adoptOptions())
	run := adoptRun(t, s, h)
	roster, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	root := roster.Members[0]
	provisioned, err := teams.Spawn(ctx, adoptDefinition(), s, h, h, h, run.ID, root.Actor, "subworker", "spawn-1", childLimits(), limits())
	must(t, err)
	adopted, err := h.Adopt(ctx, adoptRequest(run, "adopt-1", fleetActor, "fleet-session-1"))
	must(t, err)
	for _, m := range []teams.Member{provisioned, adopted, root} {
		var err error
		if m.Resolution == teams.Fresh {
			err = h.Retire(ctx, run.ID, m)
		} else {
			err = h.Release(ctx, run.ID, m)
		}
		must(t, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.stopped[provisioned.Intent.IdempotencyKey] || !f.stopped[root.Intent.IdempotencyKey] {
		t.Fatal("provisioned sessions must be stopped")
	}
	if f.stopped["adopt-1"] {
		t.Fatal("adopted session must not be stopped")
	}
	// The fleet agent is still bound in the registry's eyes: the fake adopter's
	// record is untouched and nothing released it.
	if v := a.live[fleetActor]; v.SessionID != "fleet-session-1" || !v.Resolved || v.Generation != 3 {
		t.Fatalf("adopted actor's binding changed: %+v", v)
	}
}

func TestAdoptRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeAdopter, *teamhost.AdoptRequest)
		want   error
	}{
		{"unresolved binding", func(a *fakeAdopter, _ *teamhost.AdoptRequest) {
			a.mutate(fleetActor, func(v *teamhost.Adoption) { v.Resolved = false })
		}, teams.ErrProvisionFailed},
		{"other session", func(_ *fakeAdopter, r *teamhost.AdoptRequest) { r.SessionID = "someone-elses" }, teams.ErrConflict},
		{"stale generation", func(_ *fakeAdopter, r *teamhost.AdoptRequest) { r.BindingGeneration = 2 }, teams.ErrConflict},
		{"foreign owner", func(a *fakeAdopter, _ *teamhost.AdoptRequest) {
			a.mutate(fleetActor, func(v *teamhost.Adoption) { v.Owner = "msg://user/other/x" })
		}, teams.ErrDenied},
		{"unknown launch", func(a *fakeAdopter, _ *teamhost.AdoptRequest) {
			a.mutate(fleetActor, func(v *teamhost.Adoption) { v.LaunchID = "adhoc" })
		}, teams.ErrDenied},
		{"definition pin differs from the launch record", func(a *fakeAdopter, _ *teamhost.AdoptRequest) {
			a.mutate(fleetActor, func(v *teamhost.Adoption) { v.Pin.Digest = "sha256:bbbb" })
		}, teams.ErrDenied},
		{"not an agent", func(a *fakeAdopter, _ *teamhost.AdoptRequest) {
			a.mutate(fleetActor, func(v *teamhost.Adoption) { v.Kind = mesh.ActorService })
		}, teams.ErrProvisionFailed},
		{"actor the slot does not declare", func(_ *fakeAdopter, r *teamhost.AdoptRequest) { r.Provision.Identity = "msg://agent/fleet/ghost" }, teams.ErrProvisionFailed},
		{"declared actor with no live session", func(a *fakeAdopter, _ *teamhost.AdoptRequest) {
			a.mu.Lock()
			delete(a.live, fleetActor)
			a.mu.Unlock()
		}, teams.ErrNotFound},
		{"missing generation", func(_ *fakeAdopter, r *teamhost.AdoptRequest) { r.BindingGeneration = 0 }, teams.ErrProvisionFailed},
		{"slot pin not in the run", func(_ *fakeAdopter, r *teamhost.AdoptRequest) {
			r.Provision.Slot.Definition = mesh.DefinitionRef{ID: "other", Revision: "r1", Digest: "sha256:cccc"}
		}, teams.ErrProvisionFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, a := fake(), newAdopter()
			_, s, h := openAdopt(t, t.TempDir()+"/state.db", f, a, adoptOptions())
			run := adoptRun(t, s, h)
			req := adoptRequest(run, "adopt-1", fleetActor, "fleet-session-1")
			tc.mutate(a, &req)
			if _, err := h.Adopt(ctx, req); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			// A refusal writes nothing that holds the actor: a correct retry works.
			a2 := newAdopter()
			good := adoptRequest(run, "adopt-2", fleetActor, "fleet-session-1")
			_, s2, h2 := openAdopt(t, t.TempDir()+"/second.db", fake(), a2, adoptOptions())
			if _, err := h2.Adopt(ctx, adoptRequest(adoptRun(t, s2, h2), "adopt-2", fleetActor, "fleet-session-1")); err != nil {
				t.Fatalf("control adoption: %v", err)
			}
			_ = good
		})
	}
}

func TestRefusedAdoptionFreesTheActor(t *testing.T) {
	f, a := fake(), newAdopter()
	_, s, h := openAdopt(t, t.TempDir()+"/state.db", f, a, adoptOptions())
	run := adoptRun(t, s, h)
	bad := adoptRequest(run, "adopt-bad", fleetActor, "fleet-session-1")
	bad.BindingGeneration = 9
	if _, err := h.Adopt(ctx, bad); !errors.Is(err, teams.ErrConflict) {
		t.Fatalf("stale generation: %v", err)
	}
	if _, err := h.Adopt(ctx, adoptRequest(run, "adopt-good", fleetActor, "fleet-session-1")); err != nil {
		t.Fatalf("retry after a refusal: %v", err)
	}
}

func TestAdoptRequiresAdopterAndAllowlist(t *testing.T) {
	f := fake()
	_, s, h := openAdopt(t, t.TempDir()+"/state.db", f, nil, adoptOptions())
	run := adoptRun(t, s, h)
	if _, err := h.Adopt(ctx, adoptRequest(run, "adopt-1", fleetActor, "fleet-session-1")); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatalf("no adopter: %v", err)
	}
	_, s, h = openAdopt(t, t.TempDir()+"/deny.db", fake(), newAdopter(), options())
	run = adoptRun(t, s, h)
	if _, err := h.Adopt(ctx, adoptRequest(run, "adopt-1", fleetActor, "fleet-session-1")); !errors.Is(err, teams.ErrDenied) {
		t.Fatalf("empty allowlist: %v", err)
	}
}

func TestOneActiveTeamPerActorRaceIsRefusedNotQueued(t *testing.T) {
	f, a := fake(), newAdopter()
	_, s, h := openAdopt(t, t.TempDir()+"/state.db", f, a, adoptOptions())
	run := adoptRun(t, s, h)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = h.Adopt(ctx, adoptRequest(run, "race-"+string(rune('a'+i)), fleetActor, "fleet-session-1"))
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		} else if !errors.Is(err, teams.ErrConflict) {
			t.Fatalf("loser got %v, want conflict", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d adoptions won, want exactly 1", won)
	}
}

func TestAdoptedActorCannotBeProvisionedOrAdoptedAgain(t *testing.T) {
	f, a := fake(), newAdopter()
	_, s, h := openAdopt(t, t.TempDir()+"/state.db", f, a, adoptOptions())
	run := adoptRun(t, s, h)
	_, err := h.Adopt(ctx, adoptRequest(run, "adopt-1", fleetActor, "fleet-session-1"))
	must(t, err)
	if _, err = h.Adopt(ctx, adoptRequest(run, "adopt-2", fleetActor, "fleet-session-1")); !errors.Is(err, teams.ErrConflict) {
		t.Fatalf("second team for the same actor: %v", err)
	}
	// A different actor is unaffected.
	if _, err = h.Adopt(ctx, adoptRequest(run, "adopt-3", fleetActor2, "fleet-session-2")); err != nil {
		t.Fatalf("other actor: %v", err)
	}
}

func TestAdoptIsIdempotentAndSurvivesReopen(t *testing.T) {
	path := t.TempDir() + "/state.db"
	f, a := fake(), newAdopter()
	db, s, h := openAdopt(t, path, f, a, adoptOptions())
	run := adoptRun(t, s, h)
	req := adoptRequest(run, "adopt-1", fleetActor, "fleet-session-1")
	first, err := h.Adopt(ctx, req)
	must(t, err)
	must(t, db.Close())
	_, _, h = openAdopt(t, path, f, a, adoptOptions())
	again, err := h.Adopt(ctx, req)
	must(t, err)
	if first.ID != again.ID || first.SessionID != again.SessionID || !first.JoinedAt.Equal(again.JoinedAt) {
		t.Fatalf("replay changed the member: %+v vs %+v", first, again)
	}
	changed := req
	changed.BindingGeneration = 4
	if _, err = h.Adopt(ctx, changed); err == nil {
		t.Fatal("a changed request under the same key must be refused")
	}
}

func TestReleaseWithoutAdopterFailsClosed(t *testing.T) {
	path := t.TempDir() + "/state.db"
	f, a := fake(), newAdopter()
	db, s, h := openAdopt(t, path, f, a, adoptOptions())
	run := adoptRun(t, s, h)
	member, err := h.Adopt(ctx, adoptRequest(run, "adopt-1", fleetActor, "fleet-session-1"))
	must(t, err)
	must(t, db.Close())
	_, _, h = openAdopt(t, path, f, nil, adoptOptions())
	if err = h.Release(ctx, run.ID, member); err == nil {
		t.Fatal("release without an adopter must fail, not fall back to stopping the session")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped["adopt-1"] || len(f.stopOrder) != 0 {
		t.Fatal("session stopped on the fallback path")
	}
}

func TestReboundAdoptionSuspendsUntilReadopted(t *testing.T) {
	f, a := fake(), newAdopter()
	_, s, h := openAdopt(t, t.TempDir()+"/state.db", f, a, adoptOptions())
	run := adoptRun(t, s, h)
	member, err := h.Adopt(ctx, adoptRequest(run, "adopt-1", fleetActor, "fleet-session-1"))
	must(t, err)
	must(t, h.SuspendRebound(ctx, 10))
	delivery := func(key string) teams.Delivery {
		root := teams.Member{ID: "root", Actor: operator, Kind: mesh.ActorUser, Governance: teams.Owner}
		return teams.Delivery{IdempotencyKey: key, From: operator, Recipient: member, Body: "hello", Verb: mesh.MessageSend, Route: teams.Route{RunID: run.ID, RosterVersion: 1, Sender: root, Verb: mesh.MessageSend}}
	}
	must(t, h.SendMessage(ctx, delivery("d-1")))
	a.mutate(fleetActor, func(v *teamhost.Adoption) { v.Generation = 4 })
	must(t, h.SuspendRebound(ctx, 10))
	if err = h.SendMessage(ctx, delivery("d-2")); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatalf("message to a suspended member: %v", err)
	}
	if err = h.Readopt(ctx, "adopt-1", 3); !errors.Is(err, teams.ErrConflict) {
		t.Fatalf("readopt at the old generation: %v", err)
	}
	must(t, h.Readopt(ctx, "adopt-1", 4))
	must(t, h.SendMessage(ctx, delivery("d-3")))
	// Release still never stops it.
	must(t, h.Release(ctx, run.ID, member))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.stopOrder) != 0 {
		t.Fatal("session stopped")
	}
}
