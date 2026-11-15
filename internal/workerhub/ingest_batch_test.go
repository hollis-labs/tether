package workerhub_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/workerwire"
)

// CW-20261115-0027: output frames already queued behind one being ingested go
// to EventBatch together; a state frame ends the run and goes to Event, after
// it, so order holds; a frame the batch refuses is replayed from the cursor.
func TestQueuedOutputFramesAreIngestedAsOneBatch(t *testing.T) {
	hub, db := openHub(t)
	release := make(chan struct{})
	var mu sync.Mutex
	var calls []string
	first := true
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, s)
	}
	hub.EventBatch = func(_ string, frames []workerwire.Frame) []error {
		mu.Lock()
		block := first
		first = false
		mu.Unlock()
		if block {
			<-release // the first frame stalls ingest while the rest queue up
		}
		var parts []string
		errs := make([]error, len(frames))
		for i, f := range frames {
			parts = append(parts, fmt.Sprintf("%s:%d", f.Session, f.Seq))
			if f.Session == "s-bad" {
				errs[i] = errors.New("refused")
			}
		}
		record("batch[" + strings.Join(parts, " ") + "]")
		return errs
	}
	hub.Event = func(_ string, f workerwire.Frame) error {
		record("event[" + f.Session + ":" + f.T + "]")
		return nil
	}
	r := newStallRig(t, hub, db, nil)
	send := func(f workerwire.Frame) {
		t.Helper()
		if err := r.ws.WriteJSON(f); err != nil {
			t.Fatal(err)
		}
	}
	send(workerwire.Frame{T: "output", Session: "s1", Seq: 1, Data: []byte("a")})
	time.Sleep(100 * time.Millisecond) // the first batch is now blocked on release
	send(workerwire.Frame{T: "output", Session: "s1", Seq: 2, Data: []byte("b")})
	send(workerwire.Frame{T: "output", Session: "s2", Seq: 1, Data: []byte("c")})
	send(workerwire.Frame{T: "output", Session: "s-bad", Seq: 1, Data: []byte("d")})
	send(workerwire.Frame{T: "state", Session: "s1", State: "running"})
	send(workerwire.Frame{T: "output", Session: "s1", Seq: 3, Data: []byte("e")})
	time.Sleep(100 * time.Millisecond)
	close(release)
	want := []string{"batch[s1:1]", "batch[s1:2 s2:1 s-bad:1]", "event[s1:state]", "batch[s1:3]"}
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		got := slices.Clone(calls)
		mu.Unlock()
		if slices.Equal(got, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ingest calls %v, want %v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case f := <-r.seen:
		if f.T != "replay" || f.Session != "s-bad" {
			t.Fatalf("after a refused frame the hub sent %+v, want a replay of s-bad", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no replay asked for the refused frame")
	}
}
