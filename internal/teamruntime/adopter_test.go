package teamruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
)

const (
	adoptActor   mesh.URN = "msg://agent/fleet/one"
	adoptOwner   mesh.URN = "msg://user/local/operator"
	adoptSession string   = "sess-1"
)

var adoptPin = mesh.DefinitionRef{ID: "claude-agent", Revision: "r1", Digest: "sha256:aaaa"}

type fakeDirectory struct {
	profile    AdoptionActor
	profileErr error
	binding    AdoptionBinding
	bindingErr error
}

func (d *fakeDirectory) AdoptionActor(context.Context, mesh.URN) (AdoptionActor, error) {
	return d.profile, d.profileErr
}
func (d *fakeDirectory) AdoptionBinding(context.Context, mesh.URN, time.Time) (AdoptionBinding, error) {
	return d.binding, d.bindingErr
}

type fakeSessions struct {
	row *store.SessionRow
	err error
}

func (s fakeSessions) GetSession(string) (*store.SessionRow, error) { return s.row, s.err }

type fakeOwners struct {
	owner string
	err   error
}

func (o fakeOwners) SessionOwner(context.Context, string) (string, error) { return o.owner, o.err }

type fakeNotices struct{ sent map[string]messaging.Envelope }

func (n *fakeNotices) SendKeyedMessage(_ context.Context, key string, env messaging.Envelope) (messaging.Envelope, error) {
	if n.sent == nil {
		n.sent = map[string]messaging.Envelope{}
	}
	n.sent[key] = env
	return env, nil
}

type fakeDefinitions struct{ err error }

func (d fakeDefinitions) Load(context.Context, mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error) {
	return definitionresolve.VerifiedDefinition{}, d.err
}

func healthyAdopter() (*RegistryAdopter, *fakeDirectory, *fakeNotices) {
	d := &fakeDirectory{
		profile: AdoptionActor{URN: string(adoptActor), ActiveAgent: true},
		binding: AdoptionBinding{SessionID: adoptSession, Generation: 4, Live: true},
	}
	n := &fakeNotices{}
	return &RegistryAdopter{Directory: d, Sessions: fakeSessions{row: &store.SessionRow{ID: adoptSession, LaunchID: "tether-claude", State: "running"}}, Owners: fakeOwners{owner: string(adoptOwner)}, Notices: n,
		Launches: teamhost.AdoptedLaunches{"tether-claude": adoptPin}, From: adoptOwner, Now: func() time.Time { return time.Unix(1_000, 0) }}, d, n
}

func adoptCheck() teamhost.AdoptionCheck {
	return teamhost.AdoptionCheck{Actor: adoptActor, SessionID: adoptSession, Principal: adoptOwner}
}

func TestVerifyReadsEverythingFromTheDaemonsRecords(t *testing.T) {
	a, _, _ := healthyAdopter()
	got, err := a.Verify(context.Background(), adoptCheck())
	if err != nil {
		t.Fatal(err)
	}
	want := teamhost.Adoption{Actor: adoptActor, AgentID: string(adoptActor), Kind: mesh.ActorAgent, SessionID: adoptSession, Generation: 4, Resolved: true, Owner: adoptOwner, LaunchID: "tether-claude", Pin: adoptPin}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestVerifyDoesNotResolveWhatTheRecordsDoNotProve(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*RegistryAdopter, *fakeDirectory)
		check  func(t *testing.T, got teamhost.Adoption, err error)
	}{
		"not live (no binding, revoked, expired or generation 0)": {func(_ *RegistryAdopter, d *fakeDirectory) { d.binding.Live = false }, notResolved},
		"binding no session": {func(_ *RegistryAdopter, d *fakeDirectory) { d.binding.SessionID = "" }, notResolved},
		"session missing":    {func(a *RegistryAdopter, _ *fakeDirectory) { a.Sessions = fakeSessions{err: store.ErrSessionNotFound} }, notResolved},
		"session ended": {func(a *RegistryAdopter, _ *fakeDirectory) {
			a.Sessions = fakeSessions{row: &store.SessionRow{ID: adoptSession, LaunchID: "tether-claude", State: "completed"}}
		}, notResolved},
		"no live credential (observe mode)": {func(a *RegistryAdopter, _ *fakeDirectory) { a.Owners = fakeOwners{} }, notResolved},
		"actor not registered": {func(_ *RegistryAdopter, d *fakeDirectory) { d.profileErr = teams.ErrNotFound }, func(t *testing.T, _ teamhost.Adoption, err error) {
			if !errors.Is(err, teams.ErrNotFound) {
				t.Fatalf("got %v", err)
			}
		}},
		"not an active agent": {func(_ *RegistryAdopter, d *fakeDirectory) { d.profile.ActiveAgent = false }, func(t *testing.T, _ teamhost.Adoption, err error) {
			if !errors.Is(err, teams.ErrProvisionFailed) {
				t.Fatalf("got %v", err)
			}
		}},
		"pin does not load": {func(a *RegistryAdopter, _ *fakeDirectory) {
			a.Definitions = fakeDefinitions{err: errors.New("pin mismatch")}
		}, func(t *testing.T, _ teamhost.Adoption, err error) {
			if !errors.Is(err, teams.ErrDenied) {
				t.Fatalf("got %v", err)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			a, d, _ := healthyAdopter()
			tc.mutate(a, d)
			got, err := a.Verify(context.Background(), adoptCheck())
			tc.check(t, got, err)
		})
	}
}

func notResolved(t *testing.T, got teamhost.Adoption, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Resolved {
		t.Fatalf("resolved without proof: %+v", got)
	}
}

func TestUnlistedLaunchCarriesNoPin(t *testing.T) {
	a, _, _ := healthyAdopter()
	a.Sessions = fakeSessions{row: &store.SessionRow{ID: adoptSession, LaunchID: "adhoc", State: "running"}}
	got, err := a.Verify(context.Background(), adoptCheck())
	if err != nil || got.LaunchID != "adhoc" || got.Pin != (mesh.DefinitionRef{}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestReleaseSendsOneKeyedNoticeAndTouchesNothingElse(t *testing.T) {
	a, _, n := healthyAdopter()
	m := teams.Member{ID: "m1", Actor: adoptActor, SessionID: adoptSession}
	for range 2 {
		if err := a.Release(context.Background(), "intent-1", m); err != nil {
			t.Fatal(err)
		}
	}
	if len(n.sent) != 1 {
		t.Fatalf("notices: %v", n.sent)
	}
	env, ok := n.sent["team-adopt-release:intent-1"]
	if !ok || env.To.URN() != string(adoptActor) || env.Kind != messaging.MsgKindNotice {
		t.Fatalf("notice: %+v", env)
	}
}

// The registry reads run against the real registry through the legacy adapter.
func TestAdoptionRegistryReadsThroughLegacyAdapter(t *testing.T) {
	db, s, reg := dbFixture(t, t.TempDir()+"/state.db")
	e := enroller(t, db, s, reg)
	ctx := context.Background()
	now := time.Now()
	if _, err := e.AdoptionActor(ctx, "msg://agent/fleet/ghost"); !errors.Is(err, teams.ErrNotFound) {
		t.Fatalf("unregistered actor: %v", err)
	}
	profile, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Fleet one"})
	if err != nil {
		t.Fatal(err)
	}
	actor := mesh.URN(profile.URN)
	got, err := e.AdoptionActor(ctx, actor)
	if err != nil || !got.ActiveAgent || got.URN != profile.URN {
		t.Fatalf("registered agent: %+v %v", got, err)
	}
	if b, err := e.AdoptionBinding(ctx, actor, now); err != nil || b != (AdoptionBinding{}) {
		t.Fatalf("unbound actor: %+v %v", b, err)
	}
	leased, err := reg.LeaseBinding(ctx, profile.URN, "sess-1", "fleet-host", "attempt-1", nil, registry.VisibilityPublishedLocal, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.AdoptionBinding(ctx, actor, now)
	if err != nil || !b.Live || b.SessionID != "sess-1" || b.Generation != leased.Generation || b.Generation < 1 {
		t.Fatalf("live binding: %+v %v", b, err)
	}
	if b, _ = e.AdoptionBinding(ctx, actor, now.Add(2*time.Hour)); b.Live {
		t.Fatal("an expired lease reads as live")
	}
	if err = reg.RevokeBinding(ctx, leased.ID); err != nil {
		t.Fatal(err)
	}
	if b, _ = e.AdoptionBinding(ctx, actor, now); b.Live {
		t.Fatal("a revoked binding reads as live")
	}
}
