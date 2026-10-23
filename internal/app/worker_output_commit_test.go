package app

import (
	"encoding/json"
	"testing"

	"github.com/hollis-labs/tether/internal/workerwire"
)

// CW-20261023-0018: an output frame stores the worker's resume cursor and its
// session.output event in one commit; the event is published through the outbox.
func TestWorkerOutputFrameStoresCursorAndEventTogether(t *testing.T) {
	s := workerOutputService(t)
	for i, text := range []string{"one\n", "two\n"} {
		f := workerwire.Frame{T: "output", Session: "s1", Seq: int64(i + 1), Stream: "stdout", Data: []byte(text)}
		if err := s.ingestWorkerFrame("worker", f); err != nil {
			t.Fatal(err)
		}
	}
	var last int64
	if err := s.Store.DB().QueryRow(`SELECT last_seq FROM session_worker_seq WHERE session_id='s1'`).Scan(&last); err != nil || last != 2 {
		t.Fatalf("cursor = %d err=%v, want 2", last, err)
	}
	evs, err := s.Store.ListEventsBySession("s1", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for _, e := range evs {
		if e.Kind != "session.output" {
			continue
		}
		var p struct {
			WorkerSeq int64 `json:"worker_seq"`
		}
		if json.Unmarshal([]byte(e.PayloadJSON), &p) != nil {
			t.Fatalf("payload %q", e.PayloadJSON)
		}
		seqs = append(seqs, p.WorkerSeq)
	}
	if len(seqs) != 2 || seqs[0] != 2 || seqs[1] != 1 {
		t.Fatalf("session.output worker_seqs = %v, want [2 1] (newest first)", seqs)
	}
	// A repeated frame (seq <= cursor) writes nothing more.
	if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: 2, Stream: "stdout", Data: []byte("two\n")}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Store.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE session_id='s1' AND kind='session.output'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("session.output rows after a repeated frame = %d err=%v, want 2", n, err)
	}
}

// A frame whose cursor commit failed is replayed from the old cursor. The turn-output
// step (a partial result line carried in memory, turn events) must not have run for
// it, or the replay applies the same bytes twice and the result line is lost.
func TestWorkerCursorFailureDoesNotCorruptTheCarryOnReplay(t *testing.T) {
	s := workerOutputService(t)
	data := resultLine("turn-replay", "hello")
	cut := len(data) / 2
	frame := workerwire.Frame{T: "output", Session: "s1", Seq: 1, Stream: "stdout", Data: data[:cut]}
	if _, err := s.Store.DB().Exec(`CREATE TRIGGER fail_cursor BEFORE INSERT ON session_worker_seq BEGIN SELECT RAISE(FAIL,'cursor failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.ingestWorkerFrame("worker", frame); err == nil {
		t.Fatal("injected cursor write did not fail")
	}
	if _, err := s.Store.DB().Exec(`DROP TRIGGER fail_cursor`); err != nil {
		t.Fatal(err)
	}
	if err := s.ingestWorkerFrame("worker", frame); err != nil {
		t.Fatal(err)
	}
	if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: 2, Stream: "stdout", Data: data[cut:]}); err != nil {
		t.Fatal(err)
	}
	if got := len(outputEvents(t, s)); got != 1 {
		t.Fatalf("%d turn outputs after replaying a failed frame, want 1", got)
	}
}
