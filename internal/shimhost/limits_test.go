//go:build linux

// Package shimhost contains host boundary checks.
package shimhost

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/testutil"
)

func TestJournalCapRetainsTerminalReserve(t *testing.T) {
	root := testutil.ShortDir(t)
	defer func() {
		if e := os.RemoveAll(root); e != nil {
			t.Error(e)
		}
	}()
	j, e := shim.OpenJournal(root, "urn:session:test-cap", 1, 2<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := j.Close(); e != nil {
			t.Error(e)
		}
	}()
	payload, _ := json.Marshal(map[string]string{"data": strings.Repeat("x", 64<<10)})
	event := mesh.Event{SchemaVersion: "1", ID: "test", Kind: "shim.output", Time: time.Now().UTC(), App: "test-provider", SessionID: "urn:session:test-cap", Source: mesh.EventSource{Channel: "shim", Confidence: 1}, Actor: mesh.Actor{URN: "msg://service/test/shim", Kind: mesh.ActorService}, Subject: "urn:session:test-cap", Generation: 1, ContentType: "application/json", PayloadSchema: "shim/v1", Visibility: "private", Payload: payload}
	for {
		_, e = j.Append(event, false)
		if e != nil {
			break
		}
	}
	var fault *shim.Error
	if !errors.As(e, &fault) || fault.Code != "journal_full" {
		t.Fatalf("cap error: %v", e)
	}
	event.Kind = "shim.output_gap"
	event.Payload = json.RawMessage(`{"code":"journal_full"}`)
	event.Truncated = true
	if _, e = j.Append(event, true); e != nil {
		t.Fatalf("terminal reserve: %v", e)
	}
	t.Log("ordinary append returns journal_full; reserved terminal append still succeeds")
}
func TestFrameAndChunkBounds(t *testing.T) {
	if shim.OutputChunk != 64<<10 || shim.MaxFrame != 1<<20 {
		t.Fatal("unexpected protocol limits")
	}
	var b bytes.Buffer
	f := shim.Frame{Major: 1, Type: "event", Session: "urn:session:test", Body: json.RawMessage(`{"data":"` + strings.Repeat("x", 1<<20) + `"}`)}
	var fault *shim.Error
	e := shim.WriteFrame(&b, f)
	if !errors.As(e, &fault) || fault.Code != "invalid_frame" {
		t.Fatalf("oversize accepted: %v", e)
	}
	t.Log("wire rejects >1 MiB frame; output/input chunks bounded separately at 64 KiB")
}
