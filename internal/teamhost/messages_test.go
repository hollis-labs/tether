package teamhost_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
)

// wrapper injects a race after ReplyResult's observational liveness checks but
// before the host's final acceptance transaction.
type racingSender struct {
	*teamhost.Host
	before func()
}

func (r racingSender) BindMessage(ctx context.Context, key, digest string) error {
	r.before()
	return r.Host.BindMessage(ctx, key, digest)
}

func TestReplyRetainedQueueReplayAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/state.db"
	f := fake()
	db, s, h := open(t, path, f)
	run, root := launch(t, s, h)
	worker := spawn(t, s, h, run, root, "worker")
	router := teams.Router{Roster: s, Sender: h}
	_, err := router.Send(ctx, definition(), teams.AddressRequest{RunID: run.ID, Actor: root.Actor, Address: "@worker", Body: "do work", Verb: mesh.Delegate}, "delegate")
	must(t, err)
	var key string
	must(t, db.DB().QueryRow(`SELECT delivery_key FROM team_host_deliveries`).Scan(&key))
	must(t, h.FlushMessages(ctx, 10))
	f.failOnce("deliver")
	must(t, router.ReplyResult(ctx, definition(), run.ID, key, worker.Actor, "done"))
	if err := h.FlushMessages(ctx, 10); !errors.Is(err, errLost) {
		t.Fatalf("message ack loss: %v", err)
	}
	state, err := h.DelegationState(ctx, key)
	must(t, err)
	if state != mesh.TaskCompleted {
		t.Fatal("reply acceptance did not end delegation")
	}
	must(t, db.Close())
	_, s, h = open(t, path, f)
	must(t, s.Mutate(ctx, run.ID, func(r *teams.Roster) error {
		for i := range r.Members {
			r.Members[i].Status = "stopped"
			r.Members[i].SessionID = "replacement"
		}
		return nil
	}))
	router = teams.Router{Roster: s, Sender: h}
	must(t, router.ReplyResult(ctx, definition(), run.ID, key, worker.Actor, "done"))
	if err := router.ReplyResult(ctx, definition(), run.ID, key, worker.Actor, "changed"); !errors.Is(err, teams.ErrConflict) {
		t.Fatalf("changed reply replay: %v", err)
	}
	must(t, h.FlushMessages(ctx, 10))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deliveries) != 2 {
		t.Fatal("reply replay duplicated transport")
	}
	for _, d := range f.deliveries {
		if d.Verb == mesh.Reply && (d.Delivery != mesh.DeliveryAtIdle || d.Recipient.SessionID != root.SessionID) {
			t.Fatal("reply retargeted or policy errLost")
		}
	}
}
func TestReplyAtomicallyRechecksDelegationAndPairs(t *testing.T) {
	for _, change := range []string{"delegation", "reporter-session", "delegator-status"} {
		t.Run(change, func(t *testing.T) {
			f := fake()
			db, s, h := open(t, t.TempDir()+"/state.db", f)
			run, root := launch(t, s, h)
			worker := spawn(t, s, h, run, root, "worker")
			router := teams.Router{Roster: s, Sender: h}
			_, err := router.Send(ctx, definition(), teams.AddressRequest{RunID: run.ID, Actor: root.Actor, Address: "@worker", Body: "work", Verb: mesh.Delegate}, "delegate")
			must(t, err)
			var key string
			must(t, db.DB().QueryRow(`SELECT delivery_key FROM team_host_deliveries`).Scan(&key))
			router.Sender = racingSender{Host: h, before: func() {
				if change == "delegation" {
					must(t, h.EndDelegation(ctx, key, mesh.TaskCanceled))
					return
				}
				must(t, s.Mutate(ctx, run.ID, func(r *teams.Roster) error {
					for i, m := range r.Members {
						if change == "reporter-session" && m.ID == worker.ID {
							r.Members[i].SessionID = "replacement"
						}
						if change == "delegator-status" && m.ID == root.ID {
							r.Members[i].Status = "stopped"
						}
					}
					return nil
				}))
			}}
			err = router.ReplyResult(ctx, definition(), run.ID, key, worker.Actor, "done")
			if !errors.Is(err, teams.ErrConflict) && !errors.Is(err, teams.ErrUnavailable) {
				t.Fatalf("racing reply accepted: %v", err)
			}
			var count int
			must(t, db.DB().QueryRow(`SELECT count(*) FROM team_host_deliveries`).Scan(&count))
			if count != 1 {
				t.Fatal("race accepted new result")
			}
		})
	}
}
func TestImmutableMessageBindingAndRetainedDelivery(t *testing.T) {
	f := fake()
	db, s, h := open(t, t.TempDir()+"/state.db", f)
	run, root := launch(t, s, h)
	spawn(t, s, h, run, root, "worker")
	must(t, h.BindMessage(ctx, "key", "digest"))
	must(t, h.BindMessage(ctx, "key", "digest"))
	if err := h.BindMessage(ctx, "key", "changed"); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("message digest rebound")
	}
	router := teams.Router{Roster: s, Sender: h}
	_, err := router.Send(ctx, definition(), teams.AddressRequest{RunID: run.ID, Actor: root.Actor, Address: "@worker", Body: "message", Verb: mesh.MessageAddress}, "message")
	must(t, err)
	var key string
	must(t, db.DB().QueryRow(`SELECT delivery_key FROM team_host_deliveries`).Scan(&key))
	d, err := h.GetDelivery(ctx, key)
	must(t, err)
	original, err := h.GetDelivery(ctx, key)
	must(t, err)
	d.Route.Recipients[0].SessionID = "changed"
	again, err := h.GetDelivery(ctx, key)
	must(t, err)
	if !reflect.DeepEqual(original, again) {
		t.Fatal("receipt is not detached")
	}
	changed := again
	changed.Body = "changed"
	if err := h.SendMessage(ctx, changed); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("delivery key rebound")
	}
	changed = again
	changed.IdempotencyKey = "unsupported"
	changed.Delivery = "immediate"
	if err := h.SendMessage(ctx, changed); !errors.Is(err, teams.ErrUnsupported) {
		t.Fatal("unsupported delivery accepted")
	}
	must(t, s.Mutate(ctx, run.ID, func(r *teams.Roster) error {
		for i := range r.Members {
			if r.Members[i].ID == again.Recipient.ID {
				r.Members[i].SessionID = "new-session"
			}
		}
		return nil
	}))
	changed = again
	changed.IdempotencyKey = "new"
	must(t, h.SendMessage(ctx, changed))
	retained, err := h.GetDelivery(ctx, "new")
	must(t, err)
	if retained.Recipient.SessionID != again.Recipient.SessionID {
		t.Fatal("retained delivery retargeted")
	}
	must(t, h.SendMessage(ctx, again)) // Already accepted before liveness changed.
}
