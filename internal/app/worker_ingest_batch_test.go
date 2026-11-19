package app

import (
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/workerwire"
)

func outputFrame(id string, seq int) workerwire.Frame {
	return workerwire.Frame{T: "output", Session: id, Seq: int64(seq), Stream: "stderr", Data: frameLine(id, seq)}
}

// CW-20261115-0027: a run of output frames from several sessions commits in
// one transaction, each session in its own order; duplicates are skipped.
func TestIngestWorkerOutputsCommitsABatch(t *testing.T) {
	groupCommit(t, 8, 20*time.Millisecond)
	svc, ids := ingestLoadRig(t, 3)
	var batch []workerwire.Frame
	for seq := 1; seq <= 5; seq++ {
		for _, id := range ids {
			batch = append(batch, outputFrame(id, seq))
		}
	}
	batch = append(batch, outputFrame(ids[0], 2)) // a duplicate of an accepted frame
	for i, err := range svc.ingestWorkerOutputs("w-load", batch) {
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	for _, id := range ids {
		if next := svc.Workers.NextSeq(id); next != 6 || sessionOutputCount(t, svc, id) != 5 {
			t.Fatalf("%s: next %d, events %d", id, next, sessionOutputCount(t, svc, id))
		}
	}
}

// A gap in one session fails that session's frames only; the others commit.
func TestIngestWorkerOutputsScopesAFailureToItsSession(t *testing.T) {
	svc, ids := ingestLoadRig(t, 2)
	batch := []workerwire.Frame{outputFrame(ids[0], 1), outputFrame(ids[1], 2), outputFrame(ids[0], 2), outputFrame(ids[1], 3)}
	errs := svc.ingestWorkerOutputs("w-load", batch)
	if errs[0] != nil || errs[2] != nil || errs[1] == nil || errs[3] == nil {
		t.Fatalf("errs %v", errs)
	}
	if svc.Workers.NextSeq(ids[0]) != 3 || svc.Workers.NextSeq(ids[1]) != 1 {
		t.Fatalf("cursors %d %d", svc.Workers.NextSeq(ids[0]), svc.Workers.NextSeq(ids[1]))
	}
}

// A crash between the log writes and the commit loses nothing accepted: no
// frame of the batch is accepted (cursors unchanged, no events), and the
// worker's replay from the stored cursor fills the batch, each frame once.
func TestIngestWorkerOutputsCrashMidBatchIsReplayed(t *testing.T) {
	svc, ids := ingestLoadRig(t, 2)
	// Accepted before the crash.
	for _, err := range svc.ingestWorkerOutputs("w-load", []workerwire.Frame{outputFrame(ids[0], 1), outputFrame(ids[1], 1)}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	var batch []workerwire.Frame
	for seq := 2; seq <= 4; seq++ {
		batch = append(batch, outputFrame(ids[0], seq), outputFrame(ids[1], seq))
	}
	beforeOutputCommit = func() error { return errors.New("crash") }
	t.Cleanup(func() { beforeOutputCommit = nil })
	for i, err := range svc.ingestWorkerOutputs("w-load", batch) {
		if err == nil {
			t.Fatalf("frame %d accepted although its batch never committed", i)
		}
	}
	for _, id := range ids {
		if next := svc.Workers.NextSeq(id); next != 2 || sessionOutputCount(t, svc, id) != 1 {
			t.Fatalf("%s after the crash: next %d, events %d (the frame accepted before must stay, nothing else)", id, next, sessionOutputCount(t, svc, id))
		}
	}
	// The worker replays each session from its stored cursor (seq 2).
	beforeOutputCommit = nil
	for i, err := range svc.ingestWorkerOutputs("w-load", batch) {
		if err != nil {
			t.Fatalf("replayed frame %d: %v", i, err)
		}
	}
	for _, id := range ids {
		if next := svc.Workers.NextSeq(id); next != 5 || sessionOutputCount(t, svc, id) != 4 {
			t.Fatalf("%s after the replay: next %d, events %d", id, next, sessionOutputCount(t, svc, id))
		}
	}
}
