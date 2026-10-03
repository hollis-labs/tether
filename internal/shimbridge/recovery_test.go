//go:build linux

package shimbridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/shimhost"
)

// This wire fixture models a final pipe gap without starting an escaping child.
func terminalWire(t *testing.T, gap ...bool) (string, string) {
	includeGap := len(gap) == 0 || gap[0]
	t.Helper()
	root, err := os.MkdirTemp("/var/tmp", "bw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	spec := shim.Launch{Session: "urn:session:wire", Instance: "urn:instance:wire", Generation: 1, Secret: "01234567890123456789012345678901", ControlDir: root}
	descriptor := filepath.Join(root, "launch.json")
	if err = shimhost.WritePrivateJSON(descriptor, spec); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(root, "control.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, e := listener.AcceptUnix()
			if e != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				send := func(kind string, v any) {
					raw, _ := json.Marshal(v)
					_ = shim.WriteFrame(conn, shim.Frame{Major: 1, Type: kind, Session: spec.Session, Body: raw})
				}
				send("hello", map[string]string{"nonce": "nonce", "controller_epoch": "0"})
				if _, e = shim.ReadFrame(conn); e != nil {
					return
				}
				send("hello", map[string]string{"journal": "journal", "controller_epoch": "1"})
				frame, e := shim.ReadFrame(conn)
				if e != nil {
					return
				}
				if frame.Type != "replay" {
					return
				}
				send("result", map[string]string{"high_water": "journal:3"})
				var replay struct {
					After string `json:"after_cursor"`
				}
				_ = json.Unmarshal(frame.Body, &replay)
				if replay.After == "journal:3" {
					for {
						if _, e = shim.ReadFrame(conn); e != nil {
							return
						}
					}
				}
				event := func(kind, cursor string, payload any) {
					raw, _ := json.Marshal(payload)
					send("event", map[string]any{"event": mesh.Event{Kind: kind, Cursor: cursor, Payload: raw}, "replay": true})
				}
				event("shim.output", "journal:1", map[string]string{"stream": "stdout", "encoding": "base64", "data": base64.StdEncoding.EncodeToString([]byte("final-suffix"))})
				if includeGap {
					event("shim.output_gap", "journal:2", map[string]string{"code": "descendant_holds_pipe"})
				} else {
					event("shim.state", "journal:2", map[string]string{})
				}
				event("shim.exit", "journal:3", shim.Exit{Status: 7, Cause: "descendant_holds_pipe"})
				for {
					if _, e = shim.ReadFrame(conn); e != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); workers.Wait() })
	return descriptor, filepath.Join(root, "bridge.json")
}
func openInput(t *testing.T) io.ReadCloser {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	return reader
}
func TestNonJournalGapFlushesSuffixAndPropagatesExit(t *testing.T) {
	descriptor, _ := terminalWire(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	output, diagnostics := &bytes.Buffer{}, &bytes.Buffer{}
	code, err := Run(ctx, Options{DescriptorPath: descriptor}, openInput(t), output, diagnostics)
	if err != nil || code != 7 || output.String() != "final-suffix" {
		t.Fatalf("gap/exit: code=%d output=%q err=%v", code, output.String(), err)
	}
	if !bytes.Contains(diagnostics.Bytes(), []byte("descendant_holds_pipe")) {
		t.Fatal("non-journal gap diagnostic missing")
	}
}
func TestAttachReturnsDurableTerminalExit(t *testing.T) {
	descriptor, state := terminalWire(t, false)
	first, cancel := context.WithTimeout(context.Background(), time.Second)
	code, err := Run(first, Options{DescriptorPath: descriptor}, openInput(t), io.Discard, io.Discard)
	cancel()
	if err != nil || code != 7 {
		t.Fatalf("first terminal exit: %d %v", code, err)
	}
	var saved map[string]json.RawMessage
	if err = shimhost.ReadPrivateJSON(state, 1<<20, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved["exit"]) == 0 {
		t.Error("terminal status not committed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	code, err = Run(ctx, Options{DescriptorPath: descriptor, Attach: true}, openInput(t), io.Discard, io.Discard)
	if err != nil || code != 7 {
		t.Fatalf("terminal attach: code=%d err=%v", code, err)
	}
}
func TestAttachMissingCheckpointIsRefused(t *testing.T) {
	descriptor, _ := terminalWire(t)
	_, err := Run(context.Background(), Options{DescriptorPath: descriptor, Attach: true, ExpectedJournal: "journal"}, io.NopCloser(bytes.NewReader(nil)), io.Discard, io.Discard)
	var fault *Failure
	if !errors.As(err, &fault) || fault.Code != "checkpoint_missing" {
		t.Fatalf("missing checkpoint: %v", err)
	}
}
func TestAttachJournalConflictPreservesCheckpointAndEpoch(t *testing.T) {
	f := startFixture(t, 16<<20)
	descriptor := filepath.Join(f.root, "launch.json")
	if err := saveJSON(descriptor, f.cfg.Launch); err != nil {
		t.Fatal(err)
	}
	observer, err := shim.Connect(filepath.Join(f.root, "c", "control.sock"), f.cfg.Launch.Secret, f.cfg.Launch.Session, f.cfg.Launch.Instance, "1", "observer", false)
	if err != nil {
		t.Fatal(err)
	}
	journal, epoch := observer.Journal, observer.Epoch
	_ = observer.Close()
	state := Checkpoint{Session: f.cfg.Launch.Session, Instance: f.cfg.Launch.Instance, Generation: 1, Journal: journal, Counter: 42}
	if err = saveJSON(f.cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), Options{DescriptorPath: descriptor, StatePath: f.cfg.StatePath, Attach: true, ExpectedJournal: "foreign", Takeover: true}, io.NopCloser(bytes.NewReader(nil)), io.Discard, io.Discard)
	var fault *Failure
	if !errors.As(err, &fault) || fault.Code != "journal_mismatch" {
		t.Fatalf("conflict: %v", err)
	}
	saved, err := ReadCheckpoint(f.cfg.StatePath)
	if err != nil || saved.Journal != journal || saved.Counter != 42 {
		t.Fatal("conflict rewrote checkpoint")
	}
	observer, err = shim.Connect(filepath.Join(f.root, "c", "control.sock"), f.cfg.Launch.Secret, f.cfg.Launch.Session, f.cfg.Launch.Instance, "1", "observer", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = observer.Close() }()
	if observer.Epoch != epoch {
		t.Fatal("journal conflict advanced epoch")
	}
}
func TestAttachClearsStaleCommitFile(t *testing.T) {
	descriptor, state := terminalWire(t)
	if err := saveJSON(state, Checkpoint{Session: "urn:session:wire", Instance: "urn:instance:wire", Generation: 1, Journal: "journal"}); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(filepath.Dir(state), ".commit-stale")
	if err := os.WriteFile(stale, []byte("capability"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = Run(context.Background(), Options{DescriptorPath: descriptor, Attach: true}, io.NopCloser(bytes.NewReader(nil)), io.Discard, io.Discard)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale temp file retained at attach")
	}
}

func TestAttachHostJournalPinRefusesBeforeEpochChanges(t *testing.T) {
	f := startFixture(t, 16<<20)
	descriptor := filepath.Join(f.root, "launch.json")
	if err := saveJSON(descriptor, f.cfg.Launch); err != nil {
		t.Fatal(err)
	}
	observer, err := shim.Connect(filepath.Join(f.root, "c", "control.sock"), f.cfg.Launch.Secret, f.cfg.Launch.Session, f.cfg.Launch.Instance, "1", "observer", false)
	if err != nil {
		t.Fatal(err)
	}
	epoch := observer.Epoch
	_ = observer.Close()
	state := Checkpoint{Session: f.cfg.Launch.Session, Instance: f.cfg.Launch.Instance, Generation: 1, Journal: "foreign"}
	if err = saveJSON(f.cfg.StatePath, state); err != nil {
		t.Fatal(err)
	}
	if _, err = Run(context.Background(), Options{DescriptorPath: descriptor, StatePath: f.cfg.StatePath, Attach: true, Takeover: true}, io.NopCloser(bytes.NewReader(nil)), io.Discard, io.Discard); err == nil {
		t.Fatal("wrong journal accepted")
	}
	observer, err = shim.Connect(filepath.Join(f.root, "c", "control.sock"), f.cfg.Launch.Secret, f.cfg.Launch.Session, f.cfg.Launch.Instance, "1", "observer", false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = observer.Close() }()
	if observer.Epoch != epoch {
		t.Fatal("wrong pinned host journal changed epoch")
	}
}
