package teamhost_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamstore"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
)

func TestStopFailureKeepsBindingAndRetryableReservation(t *testing.T) {
	f := fake()
	path := t.TempDir() + "/db"
	db, s, h := open(t, path, f)
	actor := mesh.URN("msg://agent/stable/one")
	f.enrolled[actor] = true
	req := direct(t, s, "old", teams.Durable, actor)
	m, err := h.Provision(ctx, req)
	must(t, err)
	f.before = func(op, key string) error {
		if op == "stop" && key == "old" {
			return errLost
		}
		return nil
	}
	if err = h.Release(ctx, "end", m); !errors.Is(err, errLost) {
		t.Fatal(err)
	}
	if f.bindings[actor] != "old" || f.sessions["old"] == "" || f.enrollmentEnded["old"] {
		t.Fatal("uncertain Stop freed identity")
	}
	next := req
	next.IdempotencyKey = "next"
	next.MemberID = "next"
	if _, err = h.Provision(ctx, next); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatalf("pending cleanup must retry: %v", err)
	}
	must(t, db.Close())
	_, _, h = open(t, path, f)
	f.before = nil
	must(t, h.ReconcileIntents(ctx, 1))
	if f.sessions["old"] != "" || f.bindings[actor] != "" {
		t.Fatal("reconcile did not perform pending side effects")
	}
	_, err = h.Provision(ctx, next)
	must(t, err)
}
func TestHostBindingGuardsWithoutPortOwnerChecks(t *testing.T) {
	f := fake()
	f.unfenced = true
	db, s, h := open(t, t.TempDir()+"/db", f)
	a := mesh.URN("msg://agent/stable/one")
	f.enrolled[a] = true
	req := direct(t, s, "first", teams.Durable, a)
	first, err := h.Provision(ctx, req)
	must(t, err)
	rival := req
	rival.IdempotencyKey = "rival"
	rival.MemberID = "rival"
	if _, err = h.Provision(ctx, rival); !errors.Is(err, teams.ErrProvisionFailed) {
		t.Fatalf("host allowed duplicate binding: %v", err)
	}
	unrelated := direct(t, s, "other", teams.Fresh, "")
	_, err = h.Provision(ctx, unrelated)
	must(t, err)
	must(t, h.Release(ctx, "end", first))
	var owner string
	must(t, db.DB().QueryRow(`SELECT intent_key FROM team_host_bindings WHERE intent_key='other'`).Scan(&owner))
	if owner != "other" {
		t.Fatal("cleanup erased unrelated binding")
	}
}
func TestCleanupRequestResolutionAndOwnerGuards(t *testing.T) {
	f := fake()
	_, s, h := open(t, t.TempDir()+"/db", f)
	req := direct(t, s, "one", teams.Fresh, "")
	m, err := h.Provision(ctx, req)
	must(t, err)
	changed := req
	changed.Limits.Budget++
	if err = h.Retire(ctx, "end", teams.Member{Intent: &changed}); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("changed cleanup request accepted", err)
	}
	if err = h.Release(ctx, "end", m); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("fresh release accepted", err)
	}
	for _, owner := range []teams.Member{{ID: "outside", Actor: "msg://user/outside", Governance: teams.MemberRole}, {ID: "outside", Actor: "msg://user/outside", Governance: teams.Owner, SessionID: "owned-elsewhere"}} {
		if err = h.Release(ctx, "end", owner); err == nil {
			t.Fatal("invalid owner exception accepted")
		}
	}
}
func TestRecordedMemberAndEnrollmentSurviveReopen(t *testing.T) {
	f := fake()
	path := t.TempDir() + "/db"
	db, s, h := open(t, path, f)
	req := direct(t, s, "one", teams.Fresh, "")
	f.before = func(op, key string) error {
		if op == "binding" {
			var payload []byte
			must(t, db.DB().QueryRow(`SELECT enrollment FROM team_host_intents WHERE intent_key=?`, key).Scan(&payload))
			if len(payload) == 0 {
				t.Fatal("enrollment not retained before binding")
			}
		}
		return nil
	}
	first, err := h.Provision(ctx, req)
	must(t, err)
	must(t, db.Close())
	_, _, h = open(t, path, f)
	f.before = func(op, key string) error { t.Fatalf("recorded member replay called %s", op); return nil }
	again, err := h.Provision(ctx, req)
	must(t, err)
	payload, err := json.Marshal(first)
	must(t, err)
	must(t, json.Unmarshal(payload, &first))
	if !reflect.DeepEqual(first, again) {
		t.Fatal("recorded member was not replayed")
	}
}
func TestHostFencesAndCompensatesWithoutSelfFencingPorts(t *testing.T) {
	for _, stage := range []string{"ensure", "binding-before", "launch"} {
		t.Run(stage, func(t *testing.T) {
			f := fake()
			f.unfenced = true
			_, s, h := open(t, t.TempDir()+"/db", f)
			req := direct(t, s, "late", teams.Fresh, "")
			fired := false
			launchCalls := 0
			f.before = func(op, key string) error {
				if op == "launch" {
					launchCalls++
				}
				if stage == "binding-before" && op == "binding" && !fired {
					fired = true
					must(t, h.Retire(ctx, "end", teams.Member{Intent: &req}))
				}
				return nil
			}
			f.hook = func(op, key string) error {
				if op == stage && !fired {
					fired = true
					e := f.enrollments[key]
					session := f.sessions[key]
					must(t, h.Retire(ctx, "end", teams.Member{Intent: &req}))
					// The deliberately non-fencing port's late effect lands after cleanup.
					if stage == "ensure" {
						f.enrolled[e.Actor] = true
					}
					if stage == "launch" {
						f.sessions[key] = session
					}
				}
				return nil
			}
			if _, err := h.Provision(ctx, req); !errors.Is(err, teams.ErrDenied) {
				t.Fatalf("late provision accepted: %v", err)
			}
			if stage == "binding-before" && launchCalls != 0 {
				t.Fatal("live tombstone guard allowed Launch")
			}
			if len(f.sessions) != 0 || len(f.bindings) != 0 || len(f.enrolled) != 0 {
				t.Fatal("late side effect was not compensated")
			}
		})
	}
}
func TestTargetOmissionDeniesWithTrustedSource(t *testing.T) {
	f := fake()
	db, s, h := open(t, t.TempDir()+"/db", f)
	_, root := launch(t, s, h)
	opts := options()
	delete(opts.TrustTiers, definition().Slots[1].Definition)
	h, err := teamhost.New(db.DB(), s, teamhost.Ports{Sessions: f, Enroller: f, Messenger: f, Channels: f}, opts)
	must(t, err)
	decision, err := h.ResolveTrust(context.Background(), root, definition().Slots[1])
	must(t, err)
	if decision != teams.TrustDeny {
		t.Fatal("missing target allowed despite trusted source")
	}
}

func TestDeliveryCursorOrderingAttemptsAndDeadLetters(t *testing.T) {
	f := fake()
	path := t.TempDir() + "/db"
	db, s, _ := open(t, path, f)
	clock := time.Now()
	h := timedHost(t, db, s, f, &clock)
	run, root := launch(t, s, h)
	worker := spawn(t, s, h, run, root, "worker")
	roster, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	route := teams.Route{RunID: run.ID, RosterVersion: roster.Version, Sender: root, Recipients: []teams.Member{worker}, Verb: mesh.MessageAddress}
	// Hash/key lexical order deliberately differs from insertion order.
	for _, key := range []string{"z-first", "a-second", "m-third"} {
		recipient := worker
		if key != "z-first" {
			recipient = root
		}
		plan := route
		plan.Recipients = []teams.Member{recipient}
		must(t, h.SendMessage(ctx, teams.Delivery{IdempotencyKey: key, Body: key, From: root.Actor, Recipient: recipient, Verb: mesh.MessageAddress, Route: plan}))
	}
	var calls []string
	f.before = func(op, key string) error {
		if op == "deliver" {
			calls = append(calls, key)
			if key == "z-first" {
				return errLost
			}
		}
		return nil
	}
	if err = h.FlushMessages(ctx, 1); !errors.Is(err, errLost) {
		t.Fatal(err)
	}
	var dispatched, attempts int
	must(t, db.DB().QueryRow(`SELECT dispatched,attempts FROM team_host_deliveries WHERE delivery_key='z-first'`).Scan(&dispatched, &attempts))
	if dispatched != 0 || attempts != 1 {
		t.Fatal("failed delivery acknowledged")
	}
	must(t, db.Close())
	db, s, _ = open(t, path, f)
	h = timedHost(t, db, s, f, &clock)
	must(t, h.FlushMessages(ctx, 1))
	must(t, h.FlushMessages(ctx, 1))
	if !reflect.DeepEqual(calls, []string{"z-first", "a-second", "m-third"}) {
		t.Fatal("recovery starvation/order", calls)
	}
	clock = clock.Add(time.Minute)
	if err = h.FlushMessages(ctx, 1); !errors.Is(err, errLost) {
		t.Fatal("failed delivery not retried", err)
	}
	f.before = func(op, key string) error {
		if op == "deliver" {
			calls = append(calls, key)
			if key == "z-first" {
				return teamhost.ErrSessionGone
			}
		}
		return nil
	}
	clock = clock.Add(time.Minute)
	if err = h.FlushMessages(ctx, 1); !errors.Is(err, teamhost.ErrSessionGone) {
		t.Fatal(err)
	}
	n := len(calls)
	must(t, h.FlushMessages(ctx, 10))
	if len(calls) != n {
		t.Fatal("terminal delivery retried or successes never acknowledged")
	}
	db, _, h = open(t, path, f)
	var dead int
	must(t, db.DB().QueryRow(`SELECT dead,attempts FROM team_host_deliveries WHERE delivery_key='z-first'`).Scan(&dead, &attempts))
	if dead != 1 || attempts != 3 {
		t.Fatal("terminal state not durable")
	}
	must(t, h.FlushMessages(ctx, 1))
	if len(calls) != n {
		t.Fatal("dead letter revived after reopen")
	}
}

func TestCleanupCursorReopenAndTerminalOwnership(t *testing.T) {
	f := fake()
	path := t.TempDir() + "/db"
	db, s, _ := open(t, path, f)
	clock := time.Now()
	h := timedHost(t, db, s, f, &clock)
	a := direct(t, s, "z-poison", teams.Fresh, "")
	poison, err := h.Provision(ctx, a)
	must(t, err)
	b := a
	b.IdempotencyKey = "a-good"
	b.MemberID = "a-good"
	good, err := h.Provision(ctx, b)
	must(t, err)
	f.before = func(op, key string) error {
		if op == "stop" {
			return errLost
		}
		return nil
	}
	if err = h.Retire(ctx, "end", poison); !errors.Is(err, errLost) {
		t.Fatal(err)
	}
	if err = h.Retire(ctx, "end", good); !errors.Is(err, errLost) {
		t.Fatal(err)
	}
	var calls []string
	f.before = func(op, key string) error {
		if op == "stop" {
			calls = append(calls, key)
			if key == "z-poison" {
				return errLost
			}
		}
		return nil
	}
	if err = h.ReconcileIntents(ctx, 1); !errors.Is(err, errLost) {
		t.Fatal(err)
	}
	must(t, db.Close())
	db, s, _ = open(t, path, f)
	h = timedHost(t, db, s, f, &clock)
	must(t, h.ReconcileIntents(ctx, 1))
	if !reflect.DeepEqual(calls, []string{"z-poison", "a-good"}) {
		t.Fatal("cleanup page starved", calls)
	}
	if f.sessions["a-good"] != "" || f.bindings[good.Actor] != "" || f.enrolled[good.Actor] {
		t.Fatal("reconcile was ineffective")
	}
	f.before = func(op, key string) error {
		if op == "stop" {
			calls = append(calls, key)
			return teamhost.ErrPermanent
		}
		return nil
	}
	clock = clock.Add(time.Minute)
	if err = h.ReconcileIntents(ctx, 1); !errors.Is(err, teamhost.ErrPermanent) {
		t.Fatal(err)
	}
	n := len(calls)
	must(t, h.ReconcileIntents(ctx, 1))
	if len(calls) != n {
		t.Fatal("terminal cleanup retried")
	}
	var dead, cleaned int
	must(t, db.DB().QueryRow(`SELECT dead,cleaned FROM team_host_intents WHERE intent_key='z-poison'`).Scan(&dead, &cleaned))
	if dead != 1 || cleaned != 0 || f.bindings[poison.Actor] != "z-poison" || f.sessions["z-poison"] == "" {
		t.Fatal("terminal cleanup discarded ownership")
	}
}
func TestReplyRecipientProjectionCannotChange(t *testing.T) {
	f := fake()
	db, s, h := open(t, t.TempDir()+"/db", f)
	run, root := launch(t, s, h)
	spawn(t, s, h, run, root, "worker")
	router := teams.Router{Roster: s, Sender: h}
	_, err := router.Send(ctx, definition(), teams.AddressRequest{RunID: run.ID, Actor: root.Actor, Address: "@worker", Body: "work", Verb: mesh.Delegate}, "delegate")
	must(t, err)
	var key string
	must(t, db.DB().QueryRow(`SELECT delivery_key FROM team_host_deliveries`).Scan(&key))
	original, err := h.GetDelivery(ctx, key)
	must(t, err)
	recipient := original.Route.Sender
	recipient.Limits.Budget++
	reply := teams.Delivery{IdempotencyKey: "forged", InReplyTo: key, Body: "result", Verb: mesh.Reply, From: original.Recipient.Actor, Recipient: recipient, Route: original.Route, History: mesh.HistoryNone, Delivery: mesh.DeliveryAtIdle}
	if err = h.SendMessage(ctx, reply); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("changed recipient projection accepted", err)
	}
}
func TestPermanentSpawnRefusalReleasesReservation(t *testing.T) {
	f := fake()
	db, s, _ := open(t, t.TempDir()+"/db", f)
	team := definition()
	team.ID = "outside-launcher"
	team.Authority.Mode = teams.DevOpen
	must(t, s.PutDefinition(ctx, team))
	run := teams.TeamRun{ID: team.ID, TeamID: team.ID, TeamVersion: 1, Status: mesh.TaskWorking}
	_, err := s.CreateRun(ctx, run)
	must(t, err)
	root := teams.Member{ID: "external-root", Slot: "root", Actor: "msg://agent/external/root", Kind: mesh.ActorAgent, Status: "active", SpawnCapable: true, Governance: teams.Owner, Limits: limits(), Budget: 100}
	must(t, s.Mutate(ctx, run.ID, func(r *teams.Roster) error { r.Members = append(r.Members, root); return nil }))
	opts := options()
	opts.ActorTrust = map[mesh.URN]teamhost.TrustTier{root.Actor: teamhost.TrustTrusted}
	h, err := teamhost.New(db.DB(), s, teamhost.Ports{Sessions: f, Enroller: f, Messenger: f, Channels: f}, opts)
	must(t, err)
	_, err = teams.Spawn(ctx, team, s, h, h, h, run.ID, root.Actor, "worker", "refused", childLimits(), limits())
	if !errors.Is(err, teams.ErrProvisionFailed) {
		t.Fatal("permanent refusal not classified", err)
	}
	roster, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	for _, m := range roster.Members {
		if m.ID != root.ID && m.Status != "failed" {
			t.Fatal("refusal retained quota", m.Status)
		}
	}
	if len(f.enrollments) != 0 || len(f.sessions) != 0 {
		t.Fatal("permanent refusal reached ports")
	}
}
func TestPoolRunSubsetIsEnforced(t *testing.T) {
	f := fake()
	_, s, h := open(t, t.TempDir()+"/db", f)
	a := mesh.URN("msg://agent/pool/one")
	f.enrolled[a] = true
	req := direct(t, s, "outside-subset", teams.Pool, a)
	must(t, s.Mutate(ctx, req.RunID, func(r *teams.Roster) error { r.PoolIdentities = map[string][]mesh.URN{"worker": {}}; return nil }))
	if _, err := h.Provision(ctx, req); !errors.Is(err, teams.ErrProvisionFailed) {
		t.Fatal("run subset not enforced", err)
	}
}

func TestRepeatedRecoveryFailuresBackOffWithoutDiscarding(t *testing.T) {
	f := fake()
	db, s, _ := open(t, t.TempDir()+"/db", f)
	clock := time.Now()
	h := timedHost(t, db, s, f, &clock)
	run, root := launch(t, s, h)
	worker := spawn(t, s, h, run, root, "worker")
	roster, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	d := teams.Delivery{IdempotencyKey: "retry-exhausted", Body: "message", From: root.Actor, Recipient: worker, Verb: mesh.MessageAddress, Route: teams.Route{RunID: run.ID, RosterVersion: roster.Version, Sender: root, Recipients: []teams.Member{worker}, Verb: mesh.MessageAddress}}
	must(t, h.SendMessage(ctx, d))
	calls := 0
	f.before = func(op, key string) error {
		if op == "deliver" {
			calls++
			return errLost
		}
		return nil
	}

	for i := 0; i < 10; i++ {
		if err = h.FlushMessages(ctx, 1); !errors.Is(err, errLost) {
			t.Fatal(err)
		}
		before := calls
		must(t, h.FlushMessages(ctx, 1))
		if calls != before {
			t.Fatal("no retry backoff")
		}
		var next int64
		var dead int
		must(t, db.DB().QueryRow(`SELECT next_attempt_at,dead FROM team_host_deliveries WHERE delivery_key=?`, d.IdempotencyKey).Scan(&next, &dead))
		delay := time.Unix(0, next).Sub(clock)
		if delay < time.Second || delay > time.Minute || dead != 0 {
			t.Fatal("unbounded backoff or transient dead letter", delay, dead)
		}
		if i >= 6 && delay != time.Minute {
			t.Fatal("backoff did not reach cap", delay)
		}
		clock = time.Unix(0, next)
	}
	f.before = nil
	must(t, h.FlushMessages(ctx, 1))
	letters, err := h.ListDeadLetters(ctx, 10)
	must(t, err)
	if len(letters) != 0 {
		t.Fatal("outage discarded messages")
	}
}
func TestPermanentSlotMismatchBeforeAnySideEffect(t *testing.T) {
	f := fake()
	_, s, h := open(t, t.TempDir()+"/db", f)
	req := direct(t, s, "mismatch", teams.Fresh, "")
	req.Slot.Name = "undeclared"
	if _, err := h.Provision(ctx, req); !errors.Is(err, teams.ErrProvisionFailed) {
		t.Fatal("slot mismatch kept reservation retryable", err)
	}
	if len(f.enrollments) != 0 {
		t.Fatal("mismatch reached ports")
	}
}

func timedHost(t *testing.T, db *store.Store, s *teamstore.Store, f *fakePorts, clock *time.Time) *teamhost.Host {
	t.Helper()
	opts := options()
	opts.Now = func() time.Time { return *clock }
	h, err := teamhost.New(db.DB(), s, teamhost.Ports{Sessions: f, Enroller: f, Messenger: f, Channels: f}, opts)
	must(t, err)
	return h
}

func TestCleanupOutageBackoffRecoveryAndAlreadyGone(t *testing.T) {
	for _, stopClass := range []error{teams.ErrDenied, teams.ErrConflict, teams.ErrUnavailable} {
		t.Run(stopClass.Error(), func(t *testing.T) {
			f := fake()
			path := t.TempDir() + "/db"
			db, s, _ := open(t, path, f)
			clock := time.Now()
			h := timedHost(t, db, s, f, &clock)
			req := direct(t, s, "outage", teams.Fresh, "")
			m, err := h.Provision(ctx, req)
			must(t, err)
			f.before = func(op, key string) error {
				if op == "stop" {
					return stopClass
				}
				return nil
			}
			if err = h.Retire(ctx, "end", m); !errors.Is(err, stopClass) {
				t.Fatal(err)
			}
			for i := 0; i < 10; i++ {
				if err = h.ReconcileIntents(ctx, 1); !errors.Is(err, stopClass) {
					t.Fatal(err)
				}
				var next int64
				var dead int
				must(t, db.DB().QueryRow(`SELECT next_attempt_at,dead FROM team_host_intents WHERE intent_key=?`, req.IdempotencyKey).Scan(&next, &dead))
				if dead != 0 || time.Unix(0, next).Sub(clock) > time.Minute {
					t.Fatal("cleanup stopped recovery")
				}
				clock = time.Unix(0, next)
			}
			must(t, db.Close())
			db, s, _ = open(t, path, f)
			h = timedHost(t, db, s, f, &clock)
			f.before = nil
			must(t, h.ReconcileIntents(ctx, 1))
			if len(f.bindings) != 0 || len(f.sessions) != 0 || len(f.enrolled) != 0 {
				t.Fatal("outage recovery stranded resources")
			}
		})
	}
	for _, gone := range []error{teams.ErrNotFound, teamhost.ErrSessionGone} {
		t.Run(gone.Error(), func(t *testing.T) {
			f := fake()
			_, s, h := open(t, t.TempDir()+"/db", f)
			req := direct(t, s, "gone", teams.Fresh, "")
			m, err := h.Provision(ctx, req)
			must(t, err)
			delete(f.sessions, req.IdempotencyKey)
			f.before = func(op, key string) error {
				if op == "stop" {
					return gone
				}
				return nil
			}
			must(t, h.Retire(ctx, "end", m))
			if len(f.bindings) != 0 || len(f.enrolled) != 0 {
				t.Fatal("already-gone Stop stranded ownership")
			}
		})
	}
}
func TestUnavailableHeadOfLineAndDeadDelegation(t *testing.T) {
	f := fake()
	path := t.TempDir() + "/db"
	db, s, _ := open(t, path, f)
	clock := time.Now()
	h := timedHost(t, db, s, f, &clock)
	run, root := launch(t, s, h)
	worker := spawn(t, s, h, run, root, "worker")
	roster, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	route := teams.Route{RunID: run.ID, RosterVersion: roster.Version, Sender: root, Recipients: []teams.Member{worker}, Verb: mesh.Delegate}
	first := teams.Delivery{IdempotencyKey: "first", Body: "first", From: root.Actor, Recipient: worker, Verb: mesh.Delegate, Route: route}
	second := first
	second.IdempotencyKey = "second"
	second.Body = "second"
	must(t, h.SendMessage(ctx, first))
	must(t, h.SendMessage(ctx, second))
	var calls []string
	f.before = func(op, key string) error {
		if op == "deliver" {
			calls = append(calls, key)
			if key == "first" {
				return teamhost.ErrSessionUnavailable
			}
		}
		return nil
	}
	if err = h.FlushMessages(ctx, 10); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatal(err)
	}
	must(t, h.FlushMessages(ctx, 10))
	if !reflect.DeepEqual(calls, []string{"first"}) {
		t.Fatal("later message overtook detached head", calls)
	}
	letters, err := h.ListDeadLetters(ctx, 10)
	must(t, err)
	if len(letters) != 0 {
		t.Fatal("detached delivery discarded")
	}
	must(t, db.Close())
	db, s, _ = open(t, path, f)
	h = timedHost(t, db, s, f, &clock)
	clock = clock.Add(time.Minute)
	f.before = func(op, key string) error {
		if op == "deliver" {
			calls = append(calls, key)
			if key == "first" {
				return teamhost.ErrSessionGone
			}
		}
		return nil
	}
	// A lost write fence after delegation mutation must roll back both outcomes.
	_, err = db.DB().Exec(`CREATE TRIGGER reject_dead_delegation AFTER UPDATE OF state ON team_host_delegations WHEN NEW.state='failed' BEGIN SELECT RAISE(ABORT,'lost dead-letter commit'); END`)
	must(t, err)
	if err = h.FlushMessages(ctx, 10); err == nil {
		t.Fatal("injected commit failure ignored")
	}
	letters, err = h.ListDeadLetters(ctx, 10)
	must(t, err)
	if len(letters) != 0 {
		t.Fatal("dead letter committed without terminal delegation")
	}
	state, err := h.DelegationState(ctx, "first")
	must(t, err)
	if state != mesh.TaskWorking {
		t.Fatal("delegation partially committed")
	}
	_, err = db.DB().Exec(`DROP TRIGGER reject_dead_delegation`)
	must(t, err)
	if err = h.FlushMessages(ctx, 10); !errors.Is(err, teamhost.ErrSessionGone) {
		t.Fatal(err)
	}
	state, err = h.DelegationState(ctx, "first")
	must(t, err)
	if state != mesh.TaskFailed {
		t.Fatal("dead delegate remained non-terminal")
	}
	must(t, h.FlushMessages(ctx, 10))
	if calls[len(calls)-1] != "second" {
		t.Fatal("dead head blocked successor")
	}
	letters, err = h.ListDeadLetters(ctx, 10)
	must(t, err)
	if len(letters) != 1 || letters[0].Key != "first" || letters[0].Error == "" {
		t.Fatal("missing operator evidence", letters)
	}
}
func TestListAndReviveDeadCleanupAcrossReopen(t *testing.T) {
	f := fake()
	path := t.TempDir() + "/db"
	db, s, h := open(t, path, f)
	req := direct(t, s, "repair", teams.Fresh, "")
	m, err := h.Provision(ctx, req)
	must(t, err)
	f.before = func(op, key string) error {
		if op == "stop" {
			return teamhost.ErrInvalidRequest
		}
		return nil
	}
	if err = h.Retire(ctx, "end", m); !errors.Is(err, teamhost.ErrInvalidRequest) {
		t.Fatal(err)
	}
	if err = h.ReconcileIntents(ctx, 1); !errors.Is(err, teamhost.ErrInvalidRequest) {
		t.Fatal(err)
	}
	must(t, db.Close())
	_, _, h = open(t, path, f)
	letters, err := h.ListDeadLetters(ctx, 10)
	must(t, err)
	if len(letters) != 1 || letters[0].Kind != "intents" || letters[0].Key != req.IdempotencyKey || letters[0].Attempts != 1 {
		t.Fatal("dead cleanup not listed", letters)
	}
	f.before = nil
	must(t, h.ReconcileIntents(ctx, 1))
	if len(f.sessions) != 1 {
		t.Fatal("dead cleanup unexpectedly retried")
	}
	must(t, h.ReviveDeadLetter(ctx, "intents", req.IdempotencyKey))
	must(t, h.ReviveDeadLetter(ctx, "intents", req.IdempotencyKey))
	must(t, h.ReconcileIntents(ctx, 1))
	if len(f.sessions) != 0 || len(f.bindings) != 0 || len(f.enrolled) != 0 {
		t.Fatal("revival did not repair cleanup")
	}
	letters, err = h.ListDeadLetters(ctx, 10)
	must(t, err)
	if len(letters) != 0 {
		t.Fatal("repaired letter remained listed")
	}
	if err = h.ReviveDeadLetter(ctx, "intents", "unknown"); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("unknown revival succeeded", err)
	}
}

func TestReviveDeliveryRetainsItsPlan(t *testing.T) {
	f := fake()
	_, s, h := open(t, t.TempDir()+"/db", f)
	run, root := launch(t, s, h)
	worker := spawn(t, s, h, run, root, "worker")
	roster, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	d := teams.Delivery{IdempotencyKey: "repair-delivery", Body: "kept", From: root.Actor, Recipient: worker, Verb: mesh.MessageAddress, Route: teams.Route{RunID: run.ID, RosterVersion: roster.Version, Sender: root, Recipients: []teams.Member{worker}, Verb: mesh.MessageAddress}}
	must(t, h.SendMessage(ctx, d))
	f.before = func(op, key string) error {
		if op == "deliver" {
			return teamhost.ErrInvalidRequest
		}
		return nil
	}
	if err = h.FlushMessages(ctx, 1); !errors.Is(err, teamhost.ErrInvalidRequest) {
		t.Fatal(err)
	}
	letters, err := h.ListDeadLetters(ctx, 10)
	must(t, err)
	if len(letters) != 1 || letters[0].Kind != "deliveries" {
		t.Fatal("dead delivery not listed")
	}
	f.before = nil
	must(t, h.ReviveDeadLetter(ctx, "deliveries", d.IdempotencyKey))
	must(t, h.FlushMessages(ctx, 1))
	payload, err := json.Marshal(d)
	must(t, err)
	must(t, json.Unmarshal(payload, &d))
	if !reflect.DeepEqual(d, f.deliveries[d.IdempotencyKey]) {
		t.Fatal("revival changed retained plan")
	}
}
func TestMalformedAndDuplicateLegacyIntentBackfill(t *testing.T) {
	db, err := sql.Open("sqlite", t.TempDir()+"/legacy.db")
	must(t, err)
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE session_groups(id TEXT PRIMARY KEY)`)
	must(t, err)
	base, err := os.ReadFile("../store/migrations/0049_team_store.sql")
	must(t, err)
	_, err = db.Exec(string(base))
	must(t, err)
	for key, payload := range map[string]string{"bad": "{broken", "one": `{"Intents":[{"Key":"shared"}]}`, "two": `{"Intents":[{"Key":"shared"}]}`, "odd": `{"Intents":["bad",{},null]}`} {
		_, err = db.Exec(`INSERT INTO team_launch_leases VALUES(?,?,1,999999999999999999)`, key, "owner")
		must(t, err)
		_, err = db.Exec(`INSERT INTO team_launches VALUES(?,'digest','working',1,?)`, key, payload)
		must(t, err)
	}
	migration, err := os.ReadFile("../store/migrations/0051_team_host.sql")
	must(t, err)
	_, err = db.Exec(string(migration))
	must(t, err)
	var key string
	must(t, db.QueryRow(`SELECT intent_key FROM team_host_launch_intents WHERE intent_key='shared'`).Scan(&key))
	if key != "shared" {
		t.Fatal("valid legacy lookup lost")
	}
	_, err = db.Exec(`UPDATE team_launches SET payload='{also broken' WHERE launch_key='bad'`)
	must(t, err)
}

func TestSuccessfulRetryPreservesRecipientDeliveryOrder(t *testing.T) {
	f := fake()
	db, s, _ := open(t, t.TempDir()+"/db", f)
	clock := time.Now()
	h := timedHost(t, db, s, f, &clock)
	run, root := launch(t, s, h)
	worker := spawn(t, s, h, run, root, "worker")
	roster, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	route := teams.Route{RunID: run.ID, RosterVersion: roster.Version, Sender: root, Recipients: []teams.Member{worker}, Verb: mesh.MessageAddress}
	for _, key := range []string{"m1", "m2"} {
		must(t, h.SendMessage(ctx, teams.Delivery{IdempotencyKey: key, Body: key, From: root.Actor, Recipient: worker, Verb: mesh.MessageAddress, Route: route}))
	}
	f.before = func(op, key string) error {
		if op == "deliver" && key == "m1" {
			return teams.ErrUnavailable
		}
		return nil
	}
	if err = h.FlushMessages(ctx, 10); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatal(err)
	}
	var delivered []string
	f.hook = func(op, key string) error {
		if op == "deliver" {
			delivered = append(delivered, key)
		}
		return nil
	}
	f.before = nil
	clock = clock.Add(time.Minute)
	must(t, h.FlushMessages(ctx, 10))
	must(t, h.FlushMessages(ctx, 10))
	if !reflect.DeepEqual(delivered, []string{"m1", "m2"}) {
		t.Fatal("retry reordered recipient delivery", delivered)
	}
}
