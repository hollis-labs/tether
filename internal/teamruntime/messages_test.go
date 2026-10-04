package teamruntime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// This effect has only the existing message queue's keyed content behavior;
// it has no team receipt, request digest or end fence.
type queueEffects struct {
	db    *store.Store
	fault error
	calls int
}

func (q *queueEffects) QueueTeamDelivery(ctx context.Context, d teams.Delivery) error {
	q.calls++
	from, err := messaging.ParseURN(string(d.From))
	if err != nil {
		return err
	}
	_, _, err = q.db.CreateRoutingReply(ctx, store.NewRoutingReply{From: from, ParentID: "team-delivery:" + d.IdempotencyKey, Body: d.Body, TargetSessionID: d.Recipient.SessionID, Actor: string(d.From), IdempotencyKey: d.IdempotencyKey})
	if err != nil {
		return err
	}
	return q.fault
}
func delivery(key string) teams.Delivery {
	return teams.Delivery{IdempotencyKey: key, From: "msg://agent/local/sender", Recipient: teams.Member{Actor: "msg://agent/local/recipient", SessionID: "retained", ID: "member", Kind: mesh.ActorAgent}, Body: "hello", Delivery: mesh.DeliveryAtIdle}
}
func TestDeliveryLostAckReplayAndChangedPlanConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, s, _ := dbFixture(t, path)
	q := &queueEffects{db: db, fault: errors.New("lost queue ack")}
	port, err := NewMessenger(db.DB(), s, q)
	check(t, err)
	in := delivery("delivery")
	if err = port.Deliver(ctx, in); err == nil {
		t.Fatal("lost ack ignored")
	}
	check(t, db.Close())
	db, s, _ = dbFixture(t, path)
	q = &queueEffects{db: db}
	port, err = NewMessenger(db.DB(), s, q)
	check(t, err)
	check(t, port.Deliver(ctx, in))
	check(t, port.Deliver(ctx, in))
	if q.calls != 1 {
		t.Fatal("completed receipt requeued", q.calls)
	}
	var count int
	check(t, db.DB().QueryRow(`SELECT COUNT(*) FROM routing_replies`).Scan(&count))
	if count != 1 {
		t.Fatal("lost ack duplicated queue")
	}
	for _, change := range []func(*teams.Delivery){func(d *teams.Delivery) { d.Body = "changed" }, func(d *teams.Delivery) { d.Recipient.SessionID = "successor" }, func(d *teams.Delivery) { d.From = "msg://agent/local/other" }, func(d *teams.Delivery) { d.Delivery = "" }} {
		changed := in
		change(&changed)
		if err = port.Deliver(ctx, changed); !errors.Is(err, teams.ErrConflict) {
			t.Fatal("changed delivery request accepted", err)
		}
	}
}
func TestChannelNameIsLazyAndAdapterConstructionIsUnregistered(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	_, err := NewSessions(db, s, &sessionEffects{db: db}, e)
	check(t, err)
	_, err = NewMessenger(db.DB(), s, unusedMessages{})
	check(t, err)
	name, err := (Channels{}).Name("run-one")
	check(t, err)
	if name != "team.run-one" {
		t.Fatal(name)
	}
	names, err := db.ListChannelNames(ctx)
	check(t, err)
	if len(names) != 0 {
		t.Fatal("construction created channel", names)
	}
	for _, table := range []string{"team_port_intents", "team_runs", "team_host_routing", "team_host_intents", "sessions", "messages"} {
		var count int
		check(t, db.DB().QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&count))
		if count != 0 {
			t.Fatal("unregistered construction performed work", table, count)
		}
	}
}
func TestReconcileRepairsPendingAcquisitionsAndOwnedOrphans(t *testing.T) {
	db, s, reg := dbFixture(t, filepath.Join(t.TempDir(), "db"))
	e := enroller(t, db, s, reg)
	effects := &sessionEffects{db: db}
	sessions, err := NewSessions(db, s, effects, e)
	check(t, err)
	messages, err := NewMessenger(db.DB(), s, &queueEffects{db: db})
	check(t, err)
	_, err = e.reserve(ctx, "enrollment", "pending", fresh("pending"))
	check(t, err)
	reconcile := Reconciler{e, sessions, messages}
	check(t, reconcile.Reconcile(ctx, 10))
	saved, err := e.read(ctx, "enrollment", "pending")
	check(t, err)
	if saved.state != "done" || len(saved.payload) == 0 {
		t.Fatal("pending acquisition not repaired")
	}
	_, err = db.DB().Exec(`DELETE FROM team_port_intents WHERE port_kind='enrollment' AND intent_key='pending'`)
	check(t, err)
	other, _, err := reg.RegisterIdempotent(ctx, "agent", registry.Profile{DisplayName: "Caller"}, "caller", "unrelated")
	check(t, err)
	check(t, reconcile.Reconcile(ctx, 10))
	profile, err := reg.LookupBy(ctx, "agent", saved.nonce, enrollmentSubstrate)
	check(t, err)
	if profile.Status != registry.StatusActive {
		t.Fatal("foreign enrollment retired")
	}
	other, err = reg.Lookup(ctx, other.URN)
	check(t, err)
	if other.Status != registry.StatusActive {
		t.Fatal("orphan cleanup touched caller enrollment")
	}
	if _, err = e.read(ctx, "enrollment", "pending"); !errors.Is(err, teams.ErrNotFound) {
		t.Fatal("foreign enrollment adopted", err)
	}
}
