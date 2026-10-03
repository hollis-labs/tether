package teamhost_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamstore"
)

func TestCallerReceiptsExactBytesFirstWinsAndReopen(t *testing.T) {
	path := t.TempDir() + "/state.db"
	f := fake()
	db, s, h := open(t, path, f)
	intent := teamhost.Intent{Scope: teamhost.Scope{Principal: "msg://user/test/operator", Verb: "delegate", Key: "one"}, Digest: "digest", Request: json.RawMessage(`{"run":"one"}`)}
	if _, err := h.GetOrCreate(ctx, intent); !errors.Is(err, teams.ErrDenied) {
		t.Fatal("unleased call accepted")
	}
	must(t, s.WithLease(ctx, teamhost.ServiceKey(intent.Scope), func(leased context.Context) error {
		first, err := h.GetOrCreate(leased, intent)
		must(t, err)
		if first.Plan != nil || first.Result != nil {
			t.Fatal("unknown call fields not nil")
		}
		altered := intent
		altered.Request = json.RawMessage(`{ "run":"one" }`)
		if _, err := h.GetOrCreate(leased, altered); !errors.Is(err, teams.ErrConflict) {
			t.Fatal("request bytes changed")
		}
		altered = intent
		altered.Digest = "other"
		if _, err := h.GetOrCreate(leased, altered); !errors.Is(err, teams.ErrConflict) {
			t.Fatal("digest changed")
		}
		must(t, h.SetPlan(leased, intent.Scope, json.RawMessage(`{"session":"retained"}`)))
		must(t, h.SetPlan(leased, intent.Scope, json.RawMessage(`{"session":"retained"}`)))
		if err := h.SetPlan(leased, intent.Scope, json.RawMessage(`{"session":"replacement"}`)); !errors.Is(err, teams.ErrConflict) {
			t.Fatal("plan rewritten")
		}
		must(t, h.Complete(leased, intent.Scope, json.RawMessage(`{"done":true}`)))
		must(t, h.Complete(leased, intent.Scope, json.RawMessage(`{"done":true}`)))
		if err := h.Complete(leased, intent.Scope, json.RawMessage(`{"done":false}`)); !errors.Is(err, teams.ErrConflict) {
			t.Fatal("result rewritten")
		}
		return nil
	}))
	must(t, db.Close())
	_, s, h = open(t, path, f)
	must(t, s.WithLease(ctx, teamhost.ServiceKey(intent.Scope), func(leased context.Context) error {
		record, err := h.GetOrCreate(leased, intent)
		must(t, err)
		if string(record.Plan) != `{"session":"retained"}` || string(record.Result) != `{"done":true}` {
			t.Fatal("receipt not recovered")
		}
		record.Intent.Request[0] = 'x'
		record.Plan[0] = 'x'
		record.Result[0] = 'x'
		again, err := h.GetOrCreate(leased, intent)
		must(t, err)
		if !reflect.DeepEqual(again.Intent, intent) || again.Plan[0] != '{' || again.Result[0] != '{' {
			t.Fatal("receipt mutation escaped")
		}
		return nil
	}))
	missing := intent.Scope
	missing.Key = "missing"
	must(t, s.WithLease(ctx, teamhost.ServiceKey(missing), func(leased context.Context) error {
		if err := h.Complete(leased, missing, json.RawMessage(`{}`)); !errors.Is(err, teams.ErrNotFound) {
			t.Fatal("orphan completion")
		}
		return nil
	}))
}
func TestCallerLeaseExpiryAndCommitFencing(t *testing.T) {
	f := fake()
	db, _, _ := open(t, t.TempDir()+"/state.db", f)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	s, err := teamstore.New(db.DB(), teamstore.Options{LeaseDuration: time.Minute, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	must(t, err)
	h, err := teamhost.New(db.DB(), s, teamhost.Ports{Sessions: f, Enroller: f, Messenger: f, Channels: f}, options())
	must(t, err)
	intent := teamhost.Intent{Scope: teamhost.Scope{Principal: "msg://user/test/operator", Verb: "form", Key: "expired"}, Digest: "digest", Request: json.RawMessage(`{}`)}
	err = s.WithLease(ctx, teamhost.ServiceKey(intent.Scope), func(leased context.Context) error {
		clock.Add(int64(2 * time.Minute))
		_, err := h.GetOrCreate(leased, intent)
		return err
	})
	if !errors.Is(err, teamstore.ErrLeaseLost) {
		t.Fatalf("expired lease accepted: %v", err)
	}
	intent.Scope.Key = "commit"
	_, err = db.DB().Exec(`CREATE TRIGGER expire_receipt AFTER INSERT ON team_host_calls BEGIN UPDATE team_launch_leases SET expires_ns=0; END`)
	must(t, err)
	err = s.WithLease(ctx, teamhost.ServiceKey(intent.Scope), func(leased context.Context) error {
		record, err := h.GetOrCreate(leased, intent)
		if !reflect.DeepEqual(record, teamhost.Record{}) {
			t.Fatal("fenced call returned populated record")
		}
		return err
	})
	if !errors.Is(err, teamstore.ErrLeaseLost) {
		t.Fatalf("commit fence: %v", err)
	}
	var count int
	must(t, db.DB().QueryRow(`SELECT count(*) FROM team_host_calls`).Scan(&count))
	if count != 0 {
		t.Fatal("fenced receipt committed")
	}
}
