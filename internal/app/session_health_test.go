package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

func TestSessionHealthObservationsReuseActivityAndRequests(t *testing.T) {
	svc, output := outputHarness(t, nil)
	ctx := context.Background()
	read := func() (at *time.Time, approval, question *bool) {
		t.Helper()
		h, err := svc.SessionHealthObservations(ctx, "s1")
		if err != nil {
			t.Fatal(err)
		}
		return h.LastActivityAt, h.PendingApproval, h.PendingQuestion
	}
	if at, approval, question := read(); at != nil || approval != nil || question != nil {
		t.Fatal("unobserved activity/requests must stay unknown")
	}
	if _, _, err := svc.Store.InsertEvent(events.ScopeSession, "s1", "session.shim_status", `{}`); err != nil {
		t.Fatal(err)
	}
	if at, _, _ := read(); at != nil {
		t.Fatal("bookkeeping was treated as runtime activity")
	}
	durable := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	if err := svc.Store.RecordSessionActivity(ctx, "s1", durable); err != nil {
		t.Fatal(err)
	}
	if at, _, _ := read(); at == nil || !at.Equal(durable) {
		t.Fatalf("durable activity: %v", at)
	}
	output.lastActivity = durable.Add(time.Minute)
	svc.turnOutputs.Store("s1", output)
	if at, _, _ := read(); at == nil || !at.Equal(output.lastActivity) {
		t.Fatalf("in-flight activity: %v", at)
	}
	request := func(turn, id, kind string, open bool, sequence uint64) {
		t.Helper()
		raw, _ := json.Marshal(store.EnvironmentRequest{TurnID: turn, RequestID: id, Kind: kind, Open: open, SourceSequence: sequence})
		if _, _, err := svc.Store.InsertEvent(events.ScopeSession, "s1", store.EnvironmentRequestKind, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	request("turn", "one", "approval", true, 1)
	request("turn", "two", "approval", true, 2)
	request("turn", "q", "question", true, 3)
	request("other-turn", "one", "", false, 4)
	request("turn", "one", "", false, 5)
	request("turn", "q", "", false, 6)
	request("turn", "q", "question", true, 3) // stale replay cannot reopen
	if _, approval, question := read(); approval == nil || !*approval || question == nil || *question {
		t.Fatalf("concurrent request state: approval=%v question=%v", approval, question)
	}
	request("turn", "two", "", false, 7)
	// A fresh service has no in-memory observer; durable request state survives.
	restarted := &Service{Store: svc.Store}
	h, err := restarted.SessionHealthObservations(ctx, "s1")
	if err != nil || h.PendingApproval == nil || *h.PendingApproval || h.PendingQuestion == nil || *h.PendingQuestion || h.LastActivityAt == nil || !h.LastActivityAt.Equal(durable) {
		t.Fatalf("restarted observations: %+v error=%v", h, err)
	}
	if _, err := restarted.SessionHealthObservations(ctx, "missing"); !errors.Is(err, store.ErrSessionNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := restarted.SessionHealthObservations(canceled, "s1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
}
