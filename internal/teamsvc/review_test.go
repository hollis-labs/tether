package teamsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
)

func TestDissolveCannotReuseRemovedActorBeforeReservation(t *testing.T) {
	f := fixtureNew(t)
	ctx := caller(verified(ownerURN))
	req := RunRequest{Key: "old", RunID: "run"}
	fail := true
	f.host.Before = func(op, _ string) error {
		if op == "roster" && fail {
			fail = false
			return errors.New("reservation failed")
		}
		return nil
	}
	_, err := f.svc.Dissolve(ctx, req)
	if err == nil {
		t.Fatal("reservation failure ignored")
	}
	f.host.Before = nil
	must(t, f.host.Mutate(context.Background(), "run", func(r *teams.Roster) error { r.Members[0].Status = "released"; return nil }))
	_, err = f.svc.Dissolve(ctx, RunRequest{Key: "fresh", RunID: "run"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	_, err = f.svc.Dissolve(ctx, req)
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("old key borrowed removed owner's authority", err)
	}
	roster, err := f.host.Snapshot(context.Background(), "run")
	must(t, err)
	if roster.Members[1].Status != "active" || roster.Members[2].Status != "active" {
		t.Fatal("old dissolve ended live workers")
	}
}

func TestVerifiedOperatorURNStillRequiresOperatorProof(t *testing.T) {
	f := fixtureNew(t)
	_, err := f.svc.Form(caller(Principal{ID: LocalOperator, Kind: mesh.ActorUser, Verified: true}), FormRequest{Key: "reserved", Team: definition()})
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("verified operator spoof accepted", err)
	}
	if len(f.journal.records) != 0 {
		t.Fatal("spoof acquired operator journal scope")
	}
}

type digestCalls struct{ *calls }

func (c digestCalls) GetOrCreate(ctx context.Context, intent Intent) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.records[intent.Scope]; ok {
		if old.Intent.Digest != intent.Digest {
			return Record{}, ErrConflict
		}
		return copyRecord(old), nil
	}
	record := Record{Intent: intent}
	c.records[intent.Scope] = copyRecord(record)
	return copyRecord(record), nil
}

type corruptCalls struct{ Calls }

func (c corruptCalls) GetOrCreate(ctx context.Context, intent Intent) (Record, error) {
	r, err := c.Calls.GetOrCreate(ctx, intent)
	r.Intent.Digest = "wrong"
	return r, err
}

func TestJournalScopeDigestAndCompletedReplay(t *testing.T) {
	t.Run("principal", func(t *testing.T) {
		f := fixtureNew(t)
		req := MessageRequest{Key: "shared", RunID: "run", Address: "@workers", Body: "work"}
		_, err := f.svc.Assign(caller(verified(ownerURN)), req)
		must(t, err)
		_, err = f.svc.Assign(caller(verified(peerURN)), req)
		if !errors.Is(err, ErrDenied) {
			t.Fatal("peer recovered owner receipt", err)
		}
	})
	t.Run("verb", func(t *testing.T) {
		f := fixtureNew(t)
		ctx := caller(verified(ownerURN))
		req := MessageRequest{Key: "shared", RunID: "run", Address: "@workers", Body: "work"}
		_, err := f.svc.Assign(ctx, req)
		must(t, err)
		_, err = f.svc.Delegate(ctx, req)
		must(t, err)
		foundAssign, foundDelegate := false, false
		for _, d := range f.host.Deliveries() {
			if d.Verb == mesh.Assign {
				foundAssign = true
			}
			if d.Verb == mesh.Delegate {
				foundDelegate = true
			}
		}
		if !foundAssign || !foundDelegate {
			t.Fatal("verbs shared a receipt")
		}
	})
	t.Run("digest only host", func(t *testing.T) {
		f := fixtureNew(t)
		f.svc.deps.Calls = digestCalls{f.journal}
		ctx := caller(verified(ownerURN))
		req := MessageRequest{Key: "digest", RunID: "run", Address: "@workers", Body: "first"}
		_, err := f.svc.Assign(ctx, req)
		must(t, err)
		req.Body = "changed"
		_, err = f.svc.Assign(ctx, req)
		if !errors.Is(err, ErrConflict) {
			t.Fatal("changed content recovered receipt", err)
		}
		req.Key = "other"
		_, err = f.svc.Assign(ctx, req)
		must(t, err)
		first := f.journal.records[Scope{Principal: ownerURN, Verb: string(mesh.Assign), Key: "digest"}].Intent.Digest
		second := f.journal.records[Scope{Principal: ownerURN, Verb: string(mesh.Assign), Key: "other"}].Intent.Digest
		if first == second {
			t.Fatal("digest did not distinguish different requests")
		}
	})
	t.Run("corrupt stored digest", func(t *testing.T) {
		f := fixtureNew(t)
		f.svc.deps.Calls = corruptCalls{f.journal}
		_, err := f.svc.Assign(caller(verified(ownerURN)), MessageRequest{Key: "wrong", RunID: "run", Address: "@workers", Body: "work"})
		if !errors.Is(err, ErrConflict) {
			t.Fatal("stored digest trusted", err)
		}
		if len(f.host.Deliveries()) != 0 {
			t.Fatal("conflicting receipt sent work")
		}
	})
	t.Run("ended member", func(t *testing.T) {
		f := fixtureNew(t)
		ctx := caller(verified(ownerURN))
		req := MessageRequest{Key: "accepted", RunID: "run", Address: "@workers", Body: "work"}
		first, err := f.svc.Assign(ctx, req)
		must(t, err)
		must(t, f.host.Mutate(context.Background(), "run", func(r *teams.Roster) error { r.Members[0].Status = "released"; return nil }))
		retry, err := f.svc.Assign(ctx, req)
		must(t, err)
		if first.DeliveryKeys[0] != retry.DeliveryKeys[0] {
			t.Fatal("completed receipt changed")
		}
	})
}

func TestRoundStallCeilingsAreBounded(t *testing.T) {
	for _, field := range []string{"rounds", "stalls"} {
		t.Run(field, func(t *testing.T) {
			f := fixtureNew(t)
			p := policy()
			if field == "rounds" {
				p.Limits.MaxRounds = 0
			} else {
				p.Limits.MaxStalls = 0
			}
			f.svc.deps.Ceilings = ceilings{policy: p}
			req := FormRequest{Key: "zero", Team: definition()}
			if field == "rounds" {
				req.Team.Policy.Spawn.MaxRounds = 1 << 30
			} else {
				req.Launch.Limits.MaxStalls = 1 << 30
			}
			_, err := f.svc.Form(caller(verified(ownerURN)), req)
			if !errors.Is(err, ErrUnavailable) {
				t.Fatal("zero meant unlimited", err)
			}
			f.svc.deps.Ceilings = ceilings{policy: policy()}
			req.Key = "positive"
			_, err = f.svc.Form(caller(verified(ownerURN)), req)
			if !errors.Is(err, ErrDenied) {
				t.Fatal("positive ceiling widened", err)
			}
		})
	}
}

func TestFormationNamedBoundaries(t *testing.T) {
	t.Run("maximum slots", func(t *testing.T) {
		f := fixtureNew(t)
		p := policy()
		p.MaxSlots = 1
		f.svc.deps.Ceilings = ceilings{policy: p}
		_, err := f.svc.Form(caller(verified(ownerURN)), FormRequest{Key: "slots", Team: definition()})
		if !errors.Is(err, ErrDenied) {
			t.Fatal(err)
		}
	})
	t.Run("owner slot mismatch", func(t *testing.T) {
		f := fixtureNew(t)
		team := definition()
		team.Phases[0].OwnerSlot = "workers"
		_, err := f.svc.Form(caller(verified(ownerURN)), FormRequest{Key: "slot", Team: team})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatal(err)
		}
	})
	t.Run("external owner room", func(t *testing.T) {
		f := fixtureNew(t)
		p := Principal{ID: LocalOperator, Kind: mesh.ActorUser, LocalOperator: true}
		_, err := f.svc.Form(caller(p), FormRequest{Key: "full", Team: definition()})
		if !errors.Is(err, ErrDenied) {
			t.Fatal("external owner appended past slot maximum", err)
		}
		if len(f.host.Provisioned()) != 0 {
			t.Fatal("full slot provisioned")
		}
	})
	t.Run("external owner governance", func(t *testing.T) {
		f := fixtureNew(t)
		p := Principal{ID: LocalOperator, Kind: mesh.ActorUser, LocalOperator: true}
		team := formationDefinition(p)
		team.ID = "external"
		result, err := f.svc.Form(caller(p), FormRequest{Key: "room", Team: team})
		must(t, err)
		r, err := f.host.Snapshot(context.Background(), result.Run.ID)
		must(t, err)
		found := false
		for _, m := range r.Members {
			if m.Actor == p.ID {
				found = true
				if m.Governance != teams.Owner || m.Intent != nil || m.SessionID != "" {
					t.Fatal("invalid external owner", m)
				}
			}
		}
		if !found {
			t.Fatal("missing external owner")
		}
	})
	t.Run("narrow launch limits", func(t *testing.T) {
		f := fixtureNew(t)
		team := definition()
		team.ID = "narrow"
		formed, err := f.svc.Form(caller(verified(ownerURN)), FormRequest{Key: "narrow", Team: team, Launch: teams.LaunchRequest{Limits: mesh.Limits{MaxDepth: 1, Budget: 10}}})
		must(t, err)
		roster, err := f.host.Snapshot(context.Background(), formed.Run.ID)
		must(t, err)
		for _, m := range roster.Members {
			if m.Limits.MaxDepth != 1 || m.Limits.Budget != 10 {
				t.Fatal("launch narrowing ignored", m.Limits)
			}
		}
	})
}

func TestStrictAuthorityOnDissolveRetry(t *testing.T) {
	f := fixtureNew(t)
	f.host.Before = func(op, _ string) error {
		if op == "released" {
			return errors.New("cleanup unavailable")
		}
		return nil
	}
	req := RunRequest{Key: "retry", RunID: "run"}
	ctx := caller(verified(ownerURN))
	_, err := f.svc.Dissolve(ctx, req)
	if err == nil {
		t.Fatal("cleanup failure ignored")
	}
	f.host.Before = nil
	f.team.Authority.Mode = teams.DevOpen
	f.svc.deps.Definitions = staticDefinition{f.team}
	_, err = f.svc.Dissolve(ctx, req)
	if !errors.Is(err, ErrDenied) {
		t.Fatal("retry accepted open authority", err)
	}
}

func TestCancelWithoutCascadeLeavesChildren(t *testing.T) {
	f := fixtureNew(t)
	ctx := caller(verified(ownerURN))
	child, err := f.svc.AddMember(ctx, AddMemberRequest{Key: "child", RunID: "run", Slot: "workers", Limits: mesh.Limits{Budget: 5}})
	must(t, err)
	_, err = f.svc.Cancel(ctx, CancelRequest{Key: "parent", RunID: "run", MemberID: "owner", Cascade: false})
	must(t, err)
	roster, err := f.host.Snapshot(context.Background(), "run")
	must(t, err)
	for _, m := range roster.Members {
		if m.ID == child.Member.ID && m.Status != "active" {
			t.Fatal("child canceled without cascade")
		}
	}
}

func TestMemberResultsContainNoHostDetails(t *testing.T) {
	f := fixtureNew(t)
	f.team.Authority.Grants = append(f.team.Authority.Grants, teams.Grant{FromActor: workerURN, Verb: teams.MayMessage, ToSlot: "owner"})
	f.svc.deps.Definitions = staticDefinition{f.team}
	must(t, f.host.Mutate(context.Background(), "run", func(r *teams.Roster) error {
		r.Members[0].Intent = &teams.ProvisionRequest{IdempotencyKey: "private-intent", ReservedIdentities: []mesh.URN{"msg://agent/private/reserved"}, Slot: teams.Slot{Workspace: map[string]string{"marker": "private-workspace"}}}
		return nil
	}))
	result, err := f.svc.Address(caller(verified(workerURN)), MessageRequest{Key: "minimal", RunID: "run", Address: "@owner", Body: "hello"})
	must(t, err)
	encoded, err := json.Marshal(result)
	must(t, err)
	for _, private := range []string{"owner-session", "private-intent", "private-workspace", "reserved", "session_id", "agent_id", "limits", "budget", "intent", "route"} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatal("host detail exposed", private, string(encoded))
		}
	}
	if len(result.Recipients) != 1 || result.Recipients[0].Actor != ownerURN {
		t.Fatal("missing membership projection")
	}
	record := f.journal.records[Scope{Principal: workerURN, Verb: string(mesh.MessageAddress), Key: "minimal"}]
	if bytes.Contains(record.Result, []byte("private-intent")) || !bytes.Contains(record.Plan, []byte("private-intent")) {
		t.Fatal("public result and retained internal plan confused")
	}
}

func TestRefusedLaunchDoesNotPersistDefinition(t *testing.T) {
	cases := map[string]func(*FormRequest){
		"unknown count": func(r *FormRequest) { r.Launch.Counts = map[string]int{"unknown": 1} },
		"unknown pool":  func(r *FormRequest) { r.Launch.PoolIdentities = map[string][]mesh.URN{"unknown": {workerURN}} },
		"nonpool":       func(r *FormRequest) { r.Launch.PoolIdentities = map[string][]mesh.URN{"owner": {ownerURN}} },
		"unlisted identity": func(r *FormRequest) {
			r.Launch.PoolIdentities = map[string][]mesh.URN{"workers": {"msg://agent/test/unlisted"}}
		},
		"duplicate identity": func(r *FormRequest) {
			r.Launch.PoolIdentities = map[string][]mesh.URN{"workers": {workerURN, workerURN}}
		},
		"below minimum": func(r *FormRequest) {
			r.Team.Slots[1].Min = 1
			r.Launch.PoolIdentities = map[string][]mesh.URN{"workers": {}}
		},
		"count exceeds selection": func(r *FormRequest) {
			r.Launch.Counts = map[string]int{"workers": 2}
			r.Launch.PoolIdentities = map[string][]mesh.URN{"workers": {workerURN}}
		},
		"empty roster": func(r *FormRequest) { r.Team.Slots[0].Min = 0; r.Launch.Counts = map[string]int{"owner": 0} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := fixtureNew(t)
			req := FormRequest{Key: "invalid", Team: definition()}
			req.Team.ID = "refused"
			mutate(&req)
			_, err := f.svc.Form(caller(verified(ownerURN)), req)
			if err == nil {
				t.Fatal("bad launch accepted")
			}
			_, err = f.host.GetDefinition(context.Background(), req.Team.ID, req.Team.Version)
			if !errors.Is(err, ErrNotFound) {
				t.Fatal("refused launch stored definition", err)
			}
			if len(f.host.Provisioned()) != 0 {
				t.Fatal("invalid launch provisioned")
			}
		})
	}
}

type failedResolver struct{ err error }

func (r failedResolver) ResolvePrincipal(context.Context) (Principal, error) {
	return Principal{}, r.err
}

type senderOnly struct{ teams.MessageSender }

func TestResolverErrorAndSenderConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  error
	}{
		{"unauthenticated", ErrUnauthenticated, ErrUnauthenticated},
		{"denied", ErrDenied, ErrDenied},
		{"bad credential or transient", errors.New("private resolver failure"), ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixtureNew(t)
			private := errors.Join(tc.cause, errors.New("private credential detail"))
			f.svc.deps.Principals = failedResolver{private}
			_, err := f.svc.Form(context.Background(), FormRequest{Key: "resolve", Team: definition()})
			if !errors.Is(err, tc.want) || errors.Is(err, private) {
				t.Fatal("resolver classification or details escaped", err)
			}
			_, bare := f.svc.principal(context.Background())
			if !errors.Is(bare, tc.want) || errors.Unwrap(bare) != nil {
				t.Fatal("resolver cause retained", bare)
			}
		})
	}
	f := fixtureNew(t)
	d := f.svc.deps
	d.Sender = senderOnly{f.host}
	_, err := New(d)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatal("sender missing delivery store accepted", err)
	}
}

func TestStrangersAndOversizedRequestsDoNotAcquireJournal(t *testing.T) {
	f := fixtureNew(t)
	_, err := f.svc.Assign(caller(verified("msg://agent/test/stranger")), MessageRequest{Key: "stranger", RunID: "run", Address: "@workers", Body: "hello"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if len(f.journal.records) != 0 {
		t.Fatal("stranger persisted an intent")
	}
	for _, mode := range []string{"key", "body", "request", "team"} {
		t.Run(mode, func(t *testing.T) {
			f := fixtureNew(t)
			ctx := caller(verified(ownerURN))
			req := MessageRequest{Key: "size", RunID: "run", Address: "@workers", Body: "hello"}
			switch mode {
			case "key":
				req.Key = strings.Repeat("k", MaxKeyBytes+1)
			case "body":
				req.Body = strings.Repeat("b", MaxBodyBytes+1)
			case "request":
				req.Address = strings.Repeat("a", MaxRequestBytes)
			}
			var err error
			if mode == "team" {
				team := definition()
				team.Name = strings.Repeat("t", MaxTeamBytes)
				_, err = f.svc.Form(ctx, FormRequest{Key: "team", Team: team})
			} else {
				_, err = f.svc.Assign(ctx, req)
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatal("oversized request accepted", err)
			}
			if len(f.journal.records) != 0 {
				t.Fatal("oversized request persisted")
			}
		})
	}
}

func TestRawBodyBoundDoesNotExpandHTML(t *testing.T) {
	for _, character := range []string{"&", "<", ">"} {
		t.Run(character, func(t *testing.T) {
			f := fixtureNew(t)
			_, err := f.svc.Assign(caller(verified(ownerURN)), MessageRequest{Key: "raw", RunID: "run", Address: "@workers", Body: strings.Repeat(character, MaxBodyBytes)})
			must(t, err)
			if len(f.host.Deliveries()) != 1 {
				t.Fatal("raw body was not delivered")
			}
		})
	}
}
