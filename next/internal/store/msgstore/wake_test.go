package msgstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/msgstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

func TestRecipientWakeAdmission(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		m := send(t, ms, clock, alice, bob)
		ok, out, err := ms.AdmitRecipientWake(ctx, m.ID)
		if err != nil || !ok || out != "" {
			t.Fatalf("first admit %v %q %v", ok, out, err)
		}
		if ok, out, _ = ms.AdmitRecipientWake(ctx, m.ID); ok || out != "" {
			t.Fatalf("second admit before outcome %v %q", ok, out)
		}
		if err := ms.CompleteRecipientWake(ctx, m.ID, `{"delivered":true}`); err != nil {
			t.Fatal(err)
		}
		if ok, out, _ = ms.AdmitRecipientWake(ctx, m.ID); ok || out != `{"delivered":true}` {
			t.Fatalf("admit after outcome %v %q", ok, out)
		}
		if got, found, _ := ms.RecipientWakeOutcome(ctx, m.ID); !found || got != `{"delivered":true}` {
			t.Fatalf("outcome %q %v", got, found)
		}
		if _, _, err := ms.AdmitRecipientWake(ctx, "nope"); !errors.Is(err, messaging.ErrNotFound) {
			t.Fatalf("admit missing: %v", err)
		}
	})
}

func TestRecordMessageWake(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		// next/ enforces SQLite foreign keys (unlike the old store, ADR 0008): parents first.
		if err := s.A().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := tx.ExecContext(ctx, tx.Dialect().Rebind(`INSERT INTO team_definitions (team_id, version, payload) VALUES ('team', 1, ?)`), []byte("{}"))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		exec(t, s, `INSERT INTO session_groups (id, created_at, updated_at) VALUES ('sg-1', ?, ?)`, "2026-11-14T00:00:00Z", "2026-11-14T00:00:00Z")
		exec(t, s, `INSERT INTO team_runs (run_id, session_group_id, team_id, team_version, payload, created_at) VALUES ('run-1', 'sg-1', 'team', 1, ?, ?)`, []byte("{}"), "2026-11-14T00:00:00Z")
		inTeam := send(t, ms, clock, alice, bob, func(e *messaging.Envelope) { e.Metadata = map[string]string{"team_run": "run-1"} })
		badRun := send(t, ms, clock, alice, bob, func(e *messaging.Envelope) { e.Metadata = map[string]string{"team_run": "run-missing"} })
		stream := send(t, ms, clock, alice, bob, func(e *messaging.Envelope) { e.Kind = "stream.chunk" })
		exec(t, s, `UPDATE messages SET metadata = 'not json' WHERE id = ?`, badRun.ID)
		for _, c := range []struct {
			id, kind, reason string
		}{
			{inTeam.ID, "wake.attempted", "busy"},
			{badRun.ID, "wake.outcome", "made-up-reason"},
			{stream.ID, "wake.outcome", ""},
			{inTeam.ID, "wake.other", ""},
			{"missing", "wake.outcome", ""},
		} {
			if err := ms.RecordMessageWake(ctx, c.id, bob, c.kind, "att-1", 2, 7, true, false, c.reason); err != nil {
				t.Fatalf("%s %s: %v", c.id, c.kind, err)
			}
		}
		r := s.C().Reader()
		rows, err := r.QueryContext(ctx, `SELECT kind, team_run_id, payload_json FROM message_event_journal WHERE kind LIKE 'wake.%' ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		type row struct{ kind, team, reason string }
		var got []row
		for rows.Next() {
			var kind, team, payload string
			if err := rows.Scan(&kind, &team, &payload); err != nil {
				t.Fatal(err)
			}
			var p map[string]any
			_ = json.Unmarshal([]byte(payload), &p)
			if p["binding_generation"] != float64(7) || p["attempt"] != float64(2) {
				t.Fatalf("payload %s", payload)
			}
			got = append(got, row{kind, team, p["reason"].(string)})
		}
		want := []row{{"wake.attempted", "run-1", "busy"}, {"wake.outcome", "", "unknown"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("journal %+v, want %+v", got, want)
		}
	})
}

func TestWakeTriage(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ms, clock := newStore(s)
		a := send(t, ms, clock, op, bob, func(e *messaging.Envelope) { e.Payload = json.RawMessage(`{"subject":"first line\nsecond"}`) })
		b := send(t, ms, clock, alice, bob)
		c := send(t, ms, clock, alice, bob, func(e *messaging.Envelope) {
			e.Payload = json.RawMessage(`{"body":"` + string(make([]byte, 0)) + `x"}`)
		})
		row, created, err := ms.TriageRecord(ctx, msgstore.TriageRow{MessageID: a.ID, Recipient: bob, Seat: "@bob", Class: "fyi", Reason: "r", Typed: true, CreatedAt: t0})
		if err != nil || !created || row.State != msgstore.TriagePending || !row.Typed || !row.CreatedAt.Equal(t0) {
			t.Fatalf("record %+v %v %v", row, created, err)
		}
		if _, created, _ = ms.TriageRecord(ctx, msgstore.TriageRow{MessageID: a.ID, Recipient: bob, Seat: "@bob", Class: "action", Reason: "again", CreatedAt: t0}); created {
			t.Fatal("second classification replaced the first")
		}
		for _, id := range []string{b.ID, c.ID} {
			if _, _, err := ms.TriageRecord(ctx, msgstore.TriageRow{MessageID: id, Recipient: bob, Seat: "@bob", Class: "action", Reason: "r", CreatedAt: t0}); err != nil {
				t.Fatal(err)
			}
		}
		if n, err := ms.TriageCoverPending(ctx, "@bob", b.ID, t0); err != nil || n != 1 {
			t.Fatalf("cover %d %v", n, err)
		}
		if got, _, _ := ms.TriageGet(ctx, c.ID); got.State != msgstore.TriageCovered {
			t.Fatalf("covered state %q", got.State)
		}
		nb := t0.Add(time.Hour)
		if err := ms.TriageDefer(ctx, b.ID, nb, t0); err != nil {
			t.Fatal(err)
		}
		if got, _, _ := ms.TriageGet(ctx, b.ID); !got.NotBefore.Equal(nb) {
			t.Fatalf("not_before %v", got.NotBefore)
		}
		if err := ms.TriageSetState(ctx, b.ID, msgstore.TriageWoken, t0); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := ms.TriageGet(ctx, "nope"); ok {
			t.Fatal("missing row found")
		}
		fyi, err := ms.TriageFYIPending(ctx, "@bob", 0)
		if err != nil || len(fyi) != 1 || fyi[0].Subject != "first line" || !fyi[0].Typed || fyi[0].From != op {
			t.Fatalf("fyi %+v %v", fyi, err)
		}
		_ = ms.MarkRead(ctx, a.ID, addr(t, bob))
		if fyi, _ = ms.TriageFYIPending(ctx, "@bob", 0); len(fyi) != 0 {
			t.Fatalf("read message still listed: %+v", fyi)
		}
		if last, _ := ms.SeatLastActionWake(ctx, "@bob"); !last.IsZero() {
			t.Fatalf("clock before set %v", last)
		}
		_ = ms.SetSeatLastActionWake(ctx, "@bob", t0)
		_ = ms.SetSeatLastActionWake(ctx, "@bob", nb)
		if last, _ := ms.SeatLastActionWake(ctx, "@bob"); !last.Equal(nb) {
			t.Fatalf("clock %v", last)
		}
	})
}
