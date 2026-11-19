package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func inputStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "inputs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func record(t *testing.T, db *Store, session, class string, accept bool) TurnInput {
	t.Helper()
	in, err := db.RecordTurnInput(context.Background(), TurnInput{SessionID: session, Source: InputChatSend, SenderURN: "msg://device/d1", SenderClass: class, Basis: BasisPrincipal})
	if err != nil {
		t.Fatal(err)
	}
	if accept {
		if err := db.SettleTurnInput(context.Background(), in.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	return in
}

// CW-20261021-0010: a clean, accepted input binds to the next turn and its class
// becomes the audience; the same turn asked twice gets the same answer; the next
// turn gets the next input.
func TestBindTurnInputProvenCleanInput(t *testing.T) {
	db := inputStore(t)
	ctx := context.Background()
	a := record(t, db, "s", ClassOperator, true)
	got, err := db.BindTurnInput(ctx, "s", "turn-1")
	if err != nil || got.Audience != ClassOperator || got.Basis != BasisPrincipal || len(got.InputIDs) != 1 || got.InputIDs[0] != a.ID || len(got.Classes) != 1 || got.Classes[0] != ClassOperator {
		t.Fatalf("clean operator input: %+v %v", got, err)
	}
	again, _ := db.BindTurnInput(ctx, "s", "turn-1")
	if again.Audience != got.Audience || again.InputIDs[0] != a.ID {
		t.Fatalf("re-binding the same turn changed the answer: %+v", again)
	}
	// A question, then the final output of the same turn, share the audience.
	if third, _ := db.BindTurnInput(ctx, "s", "turn-1"); third.Audience != ClassOperator {
		t.Fatalf("second output of the turn: %+v", third)
	}
	b := record(t, db, "s", ClassAgent, true)
	next, _ := db.BindTurnInput(ctx, "s", "turn-2")
	if next.Audience != ClassAgent || next.InputIDs[0] != b.ID {
		t.Fatalf("next turn must bind the next input: %+v", next)
	}
}

// History and provider-initiated turns have no recorded input: unknown.
func TestBindTurnInputWithNoInputIsUnknown(t *testing.T) {
	db := inputStore(t)
	got, err := db.BindTurnInput(context.Background(), "s", "turn-1")
	if err != nil || got.Audience != ClassUnknown || got.Basis != "no_input" || len(got.Classes) != 0 || len(got.InputIDs) != 0 {
		t.Fatalf("no input: %+v %v", got, err)
	}
}

// A failed or never-accepted input must never bind to a later, unrelated turn.
func TestFailedInputNeverBinds(t *testing.T) {
	db := inputStore(t)
	ctx := context.Background()
	failed := record(t, db, "s", ClassOperator, false)
	if err := db.SettleTurnInput(ctx, failed.ID, false); err != nil {
		t.Fatal(err)
	}
	// A provider-initiated turn arrives; the failed operator input must not claim it.
	got, _ := db.BindTurnInput(ctx, "s", "turn-1")
	if got.Audience != ClassUnknown || got.Basis != "no_input" || len(got.InputIDs) != 0 {
		t.Fatalf("a failed input bound to a later turn: %+v", got)
	}
	// And a later accepted input binds to its own turn, not past the failed one.
	good := record(t, db, "s", ClassAgent, true)
	got, _ = db.BindTurnInput(ctx, "s", "turn-2")
	if got.Audience != ClassAgent || got.InputIDs[0] != good.ID {
		t.Fatalf("after a failed input: %+v", got)
	}
	var state string
	if err := db.db.QueryRow(`SELECT state FROM turn_inputs WHERE input_id=?`, failed.ID).Scan(&state); err != nil || state != InputFailed {
		t.Fatalf("failed input state %q (%v)", state, err)
	}
	// A settled input is never changed again.
	if err := db.SettleTurnInput(ctx, failed.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT state FROM turn_inputs WHERE input_id=?`, failed.ID).Scan(&state); err != nil || state != InputFailed {
		t.Fatalf("a settled input changed to %q", state)
	}
}

// Every doubtful case is unknown with the reason as the basis, and exposes no
// class, so a consumer filtering on input_classes can never show a guess.
func TestDoubtfulBindingsAreUnknownAndExposeNoClass(t *testing.T) {
	ctx := context.Background()
	t.Run("submission not settled yet", func(t *testing.T) {
		db := inputStore(t)
		record(t, db, "s", ClassOperator, false) // still submitting
		got, _ := db.BindTurnInput(ctx, "s", "t1")
		if got.Audience != ClassUnknown || got.Basis != "submission_unsettled" || len(got.Classes) != 0 || len(got.InputIDs) != 1 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("queued behind another unbound input", func(t *testing.T) {
		db := inputStore(t)
		first := record(t, db, "s", ClassOperator, true)
		second := record(t, db, "s", ClassOperator, true)
		if first.Queued || !second.Queued {
			t.Fatalf("queued flags: first=%v second=%v", first.Queued, second.Queued)
		}
		// The first turn finds another input waiting: coalescing cannot be excluded.
		a, _ := db.BindTurnInput(ctx, "s", "t1")
		if a.Audience != ClassUnknown || a.Basis != "queued_inputs_present" || len(a.Classes) != 0 {
			t.Fatalf("first turn: %+v", a)
		}
		// The second was queued: its own turn is unknown too.
		b, _ := db.BindTurnInput(ctx, "s", "t2")
		if b.Audience != ClassUnknown || b.Basis != "queued_input" || len(b.Classes) != 0 || b.InputIDs[0] != second.ID {
			t.Fatalf("queued turn: %+v", b)
		}
	})
	t.Run("sender class unknown", func(t *testing.T) {
		db := inputStore(t)
		in, err := db.RecordTurnInput(ctx, TurnInput{SessionID: "s", Source: InputMailWake})
		if err != nil || in.SenderClass != ClassUnknown || in.Basis != BasisNone {
			t.Fatalf("defaults: %+v %v", in, err)
		}
		_ = db.SettleTurnInput(ctx, in.ID, true)
		got, _ := db.BindTurnInput(ctx, "s", "t1")
		if got.Audience != ClassUnknown || got.Basis != BasisNone || len(got.Classes) != 0 {
			t.Fatalf("%+v", got)
		}
	})
}

// The creation prompt of a chat-started session is settled when its launch ends.
func TestLaunchPromptInputSettlesWithTheLaunch(t *testing.T) {
	ctx := context.Background()
	for _, launched := range []bool{true, false} {
		db := inputStore(t)
		in, err := db.RecordTurnInput(ctx, TurnInput{SessionID: "s", Source: InputChatSend, SenderClass: ClassOperator, Basis: BasisPrincipal, RefID: "launch_prompt"})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SettleLaunchPromptInput(ctx, "s", launched); err != nil {
			t.Fatal(err)
		}
		want := InputFailed
		if launched {
			want = InputAccepted
		}
		var state string
		if err := db.db.QueryRow(`SELECT state FROM turn_inputs WHERE input_id=?`, in.ID).Scan(&state); err != nil || state != want {
			t.Fatalf("launched=%v: state %q, want %q (%v)", launched, state, want, err)
		}
		got, _ := db.BindTurnInput(ctx, "s", "t1")
		if launched && (got.Audience != ClassOperator || len(got.Classes) != 1) {
			t.Fatalf("an accepted launch prompt must bind to the first output: %+v", got)
		}
		if !launched && got.Audience != ClassUnknown {
			t.Fatalf("a failed launch prompt bound: %+v", got)
		}
	}
}

// Retention: turn_inputs follows the events window; recent rows stay.
func TestTurnInputsFollowEventRetention(t *testing.T) {
	db := inputStore(t)
	ctx := context.Background()
	old := record(t, db, "s", ClassOperator, true)
	if _, err := db.db.Exec(`UPDATE turn_inputs SET submitted_at=? WHERE input_id=?`, time.Now().Add(-100*24*time.Hour).UTC().Format(time.RFC3339Nano), old.ID); err != nil {
		t.Fatal(err)
	}
	recent := record(t, db, "s", ClassAgent, true)
	batch, err := db.DeleteEventHistoryBefore(ctx, "turn_inputs", time.Now().Add(-90*24*time.Hour), 100)
	if err != nil || batch.Removed != 1 {
		t.Fatalf("retention removed %d (%v)", batch.Removed, err)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM turn_inputs WHERE input_id=?`, recent.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a recent input was removed: %d %v", n, err)
	}
}

// CW-20261114-0061: a proven binding names the route its input arrived by, even
// when the sender class is unknown (a mail wake through the shared operator
// credential); a doubtful binding names none. Re-reading a bound turn agrees.
func TestBindTurnInputSourcesOnlyForAProvenBinding(t *testing.T) {
	ctx := context.Background()
	sources := func(a TurnAudience) string { return strings.Join(a.Sources, ",") }
	t.Run("mail wake with an unknown sender", func(t *testing.T) {
		db := inputStore(t)
		in, err := db.RecordTurnInput(ctx, TurnInput{SessionID: "s", Source: InputMailWake, SenderClass: ClassUnknown, Basis: BasisSharedOperator})
		if err != nil {
			t.Fatal(err)
		}
		_ = db.SettleTurnInput(ctx, in.ID, true)
		got, _ := db.BindTurnInput(ctx, "s", "t1")
		if got.Audience != ClassUnknown || got.Basis != BasisSharedOperator || len(got.Classes) != 0 || sources(got) != InputMailWake {
			t.Fatalf("%+v", got)
		}
		if again, _ := db.BindTurnInput(ctx, "s", "t1"); sources(again) != InputMailWake {
			t.Fatalf("re-read lost the source: %+v", again)
		}
	})
	t.Run("proven class", func(t *testing.T) {
		db := inputStore(t)
		record(t, db, "s", ClassOperator, true)
		if got, _ := db.BindTurnInput(ctx, "s", "t1"); sources(got) != InputChatSend {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("doubtful bindings", func(t *testing.T) {
		db := inputStore(t)
		record(t, db, "s", ClassOperator, false) // submitting
		if got, _ := db.BindTurnInput(ctx, "s", "t1"); got.Basis != "submission_unsettled" || len(got.Sources) != 0 {
			t.Fatalf("unsettled: %+v", got)
		}
		if again, _ := db.BindTurnInput(ctx, "s", "t1"); len(again.Sources) != 0 {
			t.Fatalf("re-read of an unsettled binding: %+v", again)
		}
		db = inputStore(t)
		record(t, db, "s", ClassOperator, true)
		record(t, db, "s", ClassOperator, true)
		a, _ := db.BindTurnInput(ctx, "s", "t1")
		b, _ := db.BindTurnInput(ctx, "s", "t2")
		if a.Basis != "queued_inputs_present" || len(a.Sources) != 0 || b.Basis != "queued_input" || len(b.Sources) != 0 {
			t.Fatalf("queued: %+v %+v", a, b)
		}
		if again, _ := db.BindTurnInput(ctx, "s", "t2"); len(again.Sources) != 0 {
			t.Fatalf("re-read of a queued binding: %+v", again)
		}
	})
}

// CW-20261114-0080: a proven mail-wake binding carries the triage class of the
// waking mail; an unproven binding, a non-wake input and an untriaged wake carry
// none. Re-reading a bound turn agrees.
func TestBindTurnInputTriageOnlyForAProvenMailWake(t *testing.T) {
	ctx := context.Background()
	wake := func(t *testing.T, db *Store, ref string, accept bool) {
		t.Helper()
		in, err := db.RecordTurnInput(ctx, TurnInput{SessionID: "s", Source: InputMailWake, SenderClass: ClassUnknown, Basis: BasisSharedOperator, RefID: ref})
		if err != nil {
			t.Fatal(err)
		}
		if accept {
			_ = db.SettleTurnInput(ctx, in.ID, true)
		}
	}
	triage := func(t *testing.T, db *Store, ref, class string) {
		t.Helper()
		if _, _, err := db.TriageRecord(ctx, TriageRow{MessageID: ref, Recipient: "msg://agent/x/a", Seat: "msg://agent/x/a", Class: class, Reason: "test", State: "woken", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("human wake", func(t *testing.T) {
		db := inputStore(t)
		triage(t, db, "m1", "human")
		wake(t, db, "m1", true)
		got, _ := db.BindTurnInput(ctx, "s", "t1")
		if strings.Join(got.Sources, ",") != InputMailWake || strings.Join(got.Triage, ",") != "human" {
			t.Fatalf("%+v", got)
		}
		if again, _ := db.BindTurnInput(ctx, "s", "t1"); strings.Join(again.Triage, ",") != "human" {
			t.Fatalf("re-read lost the triage: %+v", again)
		}
	})
	t.Run("untriaged wake", func(t *testing.T) {
		db := inputStore(t)
		wake(t, db, "m1", true)
		if got, _ := db.BindTurnInput(ctx, "s", "t1"); len(got.Sources) != 1 || len(got.Triage) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("not a wake", func(t *testing.T) {
		db := inputStore(t)
		triage(t, db, "m1", "action")
		in, _ := db.RecordTurnInput(ctx, TurnInput{SessionID: "s", Source: InputSessionTurn, SenderClass: ClassOperator, Basis: BasisPrincipal, RefID: "m1"})
		_ = db.SettleTurnInput(ctx, in.ID, true)
		if got, _ := db.BindTurnInput(ctx, "s", "t1"); len(got.Triage) != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("unproven bindings", func(t *testing.T) {
		db := inputStore(t)
		triage(t, db, "m1", "human")
		wake(t, db, "m1", false) // submitting
		if got, _ := db.BindTurnInput(ctx, "s", "t1"); got.Basis != "submission_unsettled" || len(got.Triage) != 0 {
			t.Fatalf("unsettled: %+v", got)
		}
		if again, _ := db.BindTurnInput(ctx, "s", "t1"); len(again.Triage) != 0 {
			t.Fatalf("re-read of an unsettled binding: %+v", again)
		}
		db = inputStore(t)
		triage(t, db, "m1", "human")
		triage(t, db, "m2", "action")
		wake(t, db, "m1", true)
		wake(t, db, "m2", true)
		a, _ := db.BindTurnInput(ctx, "s", "t1")
		b, _ := db.BindTurnInput(ctx, "s", "t2")
		if a.Basis != "queued_inputs_present" || len(a.Triage) != 0 || b.Basis != "queued_input" || len(b.Triage) != 0 {
			t.Fatalf("queued: %+v %+v", a, b)
		}
	})
}
