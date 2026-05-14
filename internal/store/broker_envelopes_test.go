package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/broker"
)

func TestEnvelopes_RoundTripAndRecipient(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "br.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	seed := []broker.Envelope{
		{ID: "e1", Sender: "alice", Recipient: "bob", MessageType: "request",
			Priority: 1, Payload: `{"hello":"world"}`, CreatedAt: "2026-04-19T09:00:00Z"},
		{ID: "e2", Sender: "carol", Recipient: "bob", MessageType: "request",
			Priority: 2, CreatedAt: "2026-04-19T09:01:00Z"},
		{ID: "e3", Sender: "alice", Recipient: "dave", MessageType: "request",
			CreatedAt: "2026-04-19T09:02:00Z"},
	}
	for _, e := range seed {
		if err := db.CreateEnvelope(e); err != nil {
			t.Fatalf("create %q: %v", e.ID, err)
		}
	}

	got, err := db.GetEnvelope("e1")
	if err != nil {
		t.Fatalf("get e1: %v", err)
	}
	if got.Sender != "alice" || got.Priority != 1 || got.Payload != `{"hello":"world"}` {
		t.Errorf("round-trip wrong: %+v", got)
	}

	// Recipient filter returns only bob's undelivered envelopes in FIFO order.
	rows, err := db.ListEnvelopesByRecipient("bob")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "e1" || rows[1].ID != "e2" {
		t.Errorf("recipient order wrong: %+v", idsOf(rows))
	}
}

// TestEnvelopes_CorrelationLookup pins the correlation-id semantics
// the sprint test plan asks for: a request + response pair sharing a
// workflow/correlation returns both in chronological order.
func TestEnvelopes_CorrelationLookup(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "corr.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	wf := "sprint-3-demo"
	corr := "conv-01"
	seed := []broker.Envelope{
		{ID: "req-1", Sender: "alice", Recipient: "bob", WorkflowID: wf, CorrelationID: corr,
			MessageType: "request", CreatedAt: "2026-04-19T09:00:00Z"},
		{ID: "res-1", Sender: "bob", Recipient: "alice", WorkflowID: wf, CorrelationID: corr,
			MessageType: "response", CreatedAt: "2026-04-19T09:00:05Z"},
		// A sibling conversation in the same workflow — must NOT be returned
		// when the correlation filter is applied.
		{ID: "other", Sender: "carol", Recipient: "dave", WorkflowID: wf, CorrelationID: "conv-99",
			MessageType: "request", CreatedAt: "2026-04-19T09:00:10Z"},
	}
	for _, e := range seed {
		if err := db.CreateEnvelope(e); err != nil {
			t.Fatalf("create %q: %v", e.ID, err)
		}
	}

	rows, err := db.ListEnvelopesByWorkflow(wf, corr)
	if err != nil {
		t.Fatalf("list by workflow+corr: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "req-1" || rows[1].ID != "res-1" {
		t.Errorf("correlation order wrong: %v", idsOf(rows))
	}

	// Without correlation filter, all workflow envelopes come back in order.
	all, err := db.ListEnvelopesByWorkflow(wf, "")
	if err != nil {
		t.Fatalf("list by workflow: %v", err)
	}
	if len(all) != 3 || all[0].ID != "req-1" || all[1].ID != "res-1" || all[2].ID != "other" {
		t.Errorf("workflow order wrong: %v", idsOf(all))
	}
}

func TestEnvelopes_GetMissing(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "nf.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := db.GetEnvelope("nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestEnvelopes_RejectsMissingRequired(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "req.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	for _, e := range []broker.Envelope{
		{ID: "", CreatedAt: "t"},
		{ID: "x", CreatedAt: ""},
	} {
		if err := db.CreateEnvelope(e); err == nil {
			t.Errorf("expected error for %+v, got nil", e)
		}
	}
}

func idsOf(es []broker.Envelope) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}
