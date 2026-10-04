//go:build !windows

// Package shimbridge adapts a hosted Claude stdio process to agentkit's pipes.
// It cannot acknowledge downstream consumption; see docs/shim-host.md.
package shimbridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/shimhost"
)

const MaxLineBytes = 64 << 20

// The carry may contain a full line plus the remainder of one shim read.
const maxCarryBytes = MaxLineBytes + shim.OutputChunk + 1
const maxStateBytes = (maxCarryBytes+MaxLineBytes+1)*4/3 + 65536

type Failure struct {
	Code    string
	Message string
}

func (e *Failure) Error() string     { return e.Code + ": " + e.Message }
func failure(code, msg string) error { return &Failure{Code: code, Message: msg} }

type Checkpoint struct {
	ControllerEpoch string     `json:"controller_epoch,omitempty"`
	Cursor          string     `json:"cursor"`
	Journal         string     `json:"journal"`
	Counter         uint64     `json:"inject_counter,string"`
	Partial         []byte     `json:"partial,omitempty"`
	Init            []byte     `json:"init,omitempty"`
	Session         string     `json:"session"`
	Instance        string     `json:"instance"`
	Generation      uint64     `json:"generation,string"`
	Exit            *shim.Exit `json:"exit,omitempty"`
}

// AttachEvent describes catch-up once per attach, even with zero replayed events.
type AttachEvent struct {
	Type           string `json:"type"`
	Journal        string `json:"journal"`
	Epoch          string `json:"controller_epoch"`
	ReplayedEvents uint64 `json:"replayed_events"`
	Delivery       string `json:"delivery"`
}
type Options struct {
	DescriptorPath     string
	StatePath          string
	Attach             bool
	ExpectedJournal    string
	ExpectedSession    string
	ExpectedInstance   string
	ExpectedGeneration uint64
	AfterCursor        string
	Takeover           bool
	OnAttach           func(AttachEvent) error
}
type stateStore struct {
	mu    sync.Mutex
	path  string
	value Checkpoint
}

func ReadCheckpoint(path string) (Checkpoint, error) {
	var s Checkpoint
	err := shimhost.ReadPrivateJSON(path, maxStateBytes, &s)
	return s, err
}
func (s *stateStore) update(fn func(*Checkpoint)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.value
	fn(&next)
	if err := shimhost.WritePrivateJSON(s.path, next); err != nil {
		return err
	}
	s.value = next
	return nil
}
func (s *stateStore) next() (string, error) {
	var n uint64
	err := s.update(func(v *Checkpoint) {
		if v.Counter != math.MaxUint64 {
			v.Counter++
			n = v.Counter
		}
	})
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", failure("counter_exhausted", "input operation counter exhausted")
	}
	return "bridge-" + strconv.FormatUint(n, 10), nil
}

// Run owns and closes input on return. EOF is detach, never provider kill.
// Only authenticated inject requests affect the child; uncertain effects fail.
func Run(ctx context.Context, opts Options, input io.ReadCloser, out, errout io.Writer) (int, error) {
	return run(ctx, opts, input, out, errout, nil)
}
func run(ctx context.Context, opts Options, input io.ReadCloser, out, errout io.Writer, afterWrite func([]byte)) (code int, runErr error) {
	// os.Stdin can be an inherited blocking fd, whose Close cannot interrupt
	// an ongoing syscall read. Duplicate it as a pollable, owned file first.
	if file, ok := input.(*os.File); ok {
		fd, e := syscall.Dup(int(file.Fd()))
		if e != nil {
			return 0, e
		}
		syscall.CloseOnExec(fd)
		if e = syscall.SetNonblock(fd, true); e != nil {
			_ = syscall.Close(fd)
			return 0, e
		}
		if e = input.Close(); e != nil {
			_ = syscall.Close(fd)
			return 0, e
		}
		input = os.NewFile(uintptr(fd), file.Name())
	}
	defer func() { _ = input.Close() }()
	spec, err := shimhost.ReadDescriptor(opts.DescriptorPath)
	if err != nil {
		return 0, failure("descriptor_invalid", "cannot read private launch descriptor")
	}
	if opts.StatePath == "" {
		opts.StatePath = filepath.Join(filepath.Dir(opts.DescriptorPath), "bridge.json")
	}
	lock, err := shimhost.Lock(opts.StatePath + ".lock")
	if err != nil {
		return 0, failure("bridge_busy", "checkpoint has another owner")
	}
	defer func() { _ = lock.Close() }()
	if err = shimhost.ClearCommitTemps(filepath.Dir(opts.StatePath)); err != nil {
		return 0, err
	}
	state, err := ReadCheckpoint(opts.StatePath)
	if os.IsNotExist(err) {
		if opts.Attach {
			return 0, failure("checkpoint_missing", "attach requires the durable checkpoint; explicit recovery is required")
		}
		state = Checkpoint{Session: spec.Session, Instance: spec.Instance, Generation: spec.Generation}
	} else if err != nil {
		return 0, failure("checkpoint_invalid", "cannot read private checkpoint")
	}
	if state.Session != spec.Session || state.Instance != spec.Instance || state.Generation != spec.Generation || opts.ExpectedSession != "" && opts.ExpectedSession != spec.Session || opts.ExpectedInstance != "" && opts.ExpectedInstance != spec.Instance || opts.ExpectedGeneration != 0 && opts.ExpectedGeneration != spec.Generation {
		return 0, failure("identity_mismatch", "attach identity differs from descriptor/checkpoint")
	}
	if len(state.Partial) > maxCarryBytes || len(state.Init) > MaxLineBytes+1 {
		return 0, failure("line_too_long", "stored stdout carry exceeds limit")
	}
	if opts.ExpectedJournal != "" && state.Journal != "" && opts.ExpectedJournal != state.Journal {
		return 0, failure("journal_mismatch", "requested journal differs from checkpoint")
	}
	expected := opts.ExpectedJournal
	if expected == "" {
		expected = state.Journal
	}
	if opts.Attach && expected == "" {
		return 0, failure("journal_mismatch", "attach requires expected journal")
	}
	if opts.AfterCursor != "" && opts.AfterCursor != state.Cursor {
		return 0, failure("cursor_mismatch", "cursor differs from partial-line checkpoint")
	}
	generation := strconv.FormatUint(spec.Generation, 10)
	c, err := shimhost.Connect(ctx, filepath.Join(spec.ControlDir, "control.sock"), spec.Secret, spec.Session, spec.Instance, generation, "controller", expected, opts.Takeover)
	if err != nil {
		var fault *shimhost.Failure
		if errors.As(err, &fault) {
			return 0, failure(fault.Code, "cannot verify pinned shim")
		}
		return 0, failure("host_unreachable", "cannot verify shim")
	}
	defer func() { _ = c.Close() }()
	if expected != "" && c.Journal != expected {
		return 0, failure("journal_mismatch", "refusing another shim journal")
	}
	store := &stateStore{path: opts.StatePath, value: state}
	if err = store.update(func(v *Checkpoint) {
		v.Journal = c.Journal
		v.ControllerEpoch = c.Epoch
	}); err != nil {
		return 0, err
	}
	if opts.Attach && len(state.Init) > 0 {
		if err = writeAll(out, state.Init); err != nil {
			return 0, err
		}
	}
	// Saved carry may include COMPLETE un-emitted lines from a multi-line chunk.
	// Drain these before replay; the cursor already covers that source event.
	checkpoint := func() error {
		return store.update(func(v *Checkpoint) {
			v.Cursor = state.Cursor
			v.Partial = bytes.Clone(state.Partial)
			v.Init = bytes.Clone(state.Init)
			v.Exit = state.Exit
		})
	}
	drain := func() error { return drainLines(&state, MaxLineBytes, out, checkpoint, afterWrite) }
	if err = drain(); err != nil {
		return 0, err
	}
	if state.Exit != nil {
		if opts.Attach && opts.OnAttach != nil {
			if err = opts.OnAttach(AttachEvent{Type: "shim.attach", Journal: c.Journal, Epoch: c.Epoch, Delivery: "write-before-commit; crash may duplicate an uncommitted line or lose unread pipe bytes"}); err != nil {
				return 0, err
			}
		}
		return providerExitCode(*state.Exit), nil
	}
	if err = c.Replay(spec.Session, state.Cursor); err != nil {
		return 0, err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	replies := make(chan shim.Frame, 1)
	inputErr := make(chan error, 1)
	report := func(err error) {
		select {
		case inputErr <- err:
		case <-workerCtx.Done():
		}
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		b := make([]byte, shim.OutputChunk)
		for {
			n, readErr := input.Read(b)
			if n > 0 {
				key, err := store.next()
				if err != nil {
					report(err)
					return
				}
				inj := shim.Inject{Key: key, Actor: spec.Actor, Subject: spec.Subject, Mode: "input", Delivery: "immediate", Generation: generation, Data: base64.StdEncoding.EncodeToString(b[:n])}
				raw, _ := json.Marshal(inj)
				if err = c.SendFrame(shim.Frame{Major: shim.ProtocolMajor, Type: "inject", RequestID: key, Session: spec.Session, Epoch: c.Epoch, Body: raw}); err != nil {
					report(failure("outcome_unknown", "input submit response unavailable"))
					return
				}
				var f shim.Frame
				select {
				case f = <-replies:
				case <-workerCtx.Done():
					return
				}
				var receipt struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(f.Body, &receipt) != nil {
					report(failure("outcome_unknown", "invalid input receipt"))
					return
				}
				if receipt.Code != "bytes_written" || f.Type == "error" {
					report(failure(receipt.Code, "input effect refused or uncertain; never retried"))
					return
				}
			}
			if readErr != nil {
				report(readErr)
				return
			}
		}
	}()
	defer func() { cancel(); _ = input.Close(); _ = c.Close(); workers.Wait() }()
	var replayed uint64
	high := ""
	attached := false
	emitAttach := func() error {
		if !opts.Attach || attached {
			return nil
		}
		attached = true
		if opts.OnAttach != nil {
			return opts.OnAttach(AttachEvent{Type: "shim.attach", Journal: c.Journal, Epoch: c.Epoch, ReplayedEvents: replayed, Delivery: "write-before-commit; crash may duplicate an uncommitted line or lose unread pipe bytes"})
		}
		return nil
	}
	defer func() {
		if err := emitAttach(); runErr == nil && err != nil {
			runErr = err
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case err := <-inputErr:
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return 0, emitAttach()
			}
			return 0, err
		case f, ok := <-c.Frames:
			if !ok {
				if ctx.Err() != nil {
					return 0, ctx.Err()
				}
				return 0, failure("host_unreachable", "shim disconnected")
			}
			if strings.HasPrefix(f.ReplyTo, "bridge-") {
				select {
				case replies <- f:
				case <-ctx.Done():
					return 0, ctx.Err()
				}
				continue
			}
			if f.Type == "error" {
				var e struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(f.Body, &e) != nil {
					return 0, failure("invalid_frame", "invalid shim failure")
				}
				return 0, failure(e.Code, "shim refused bridge request")
			}
			if f.Type == "result" {
				var result struct {
					High string `json:"high_water"`
				}
				if json.Unmarshal(f.Body, &result) == nil && result.High != "" {
					high = result.High
					if high == state.Cursor {
						if err = emitAttach(); err != nil {
							return 0, err
						}
					}
				}
				continue
			}
			if f.Type != "event" {
				continue
			}
			var body struct {
				Event  mesh.Event `json:"event"`
				Replay bool       `json:"replay"`
			}
			if err = json.Unmarshal(f.Body, &body); err != nil {
				return 0, failure("invalid_frame", "invalid event envelope")
			}
			ev := body.Event
			if body.Replay {
				replayed++
			}
			if ev.Kind == "shim.output_gap" {
				var gap struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(ev.Payload, &gap) != nil {
					return 0, failure("invalid_frame", "invalid output gap")
				}
				if gap.Code == "journal_unavailable" || gap.Code == "journal_full" {
					return 0, failure("journal_unavailable", "journal exhausted or unwritable; output truncated")
				}
				diagnostic, _ := json.Marshal(struct {
					Type   string `json:"type"`
					Code   string `json:"code"`
					Cursor string `json:"cursor"`
				}{"shim.output_gap", gap.Code, ev.Cursor})
				if err = writeAll(errout, append(diagnostic, '\n')); err != nil {
					return 0, err
				}
			}
			state.Cursor = ev.Cursor
			if ev.Kind == "shim.output" {
				var payload struct {
					Stream   string `json:"stream"`
					Encoding string `json:"encoding"`
					Data     string `json:"data"`
				}
				if json.Unmarshal(ev.Payload, &payload) != nil || payload.Encoding != "base64" {
					return 0, failure("invalid_frame", "invalid output payload")
				}
				data, e := base64.StdEncoding.DecodeString(payload.Data)
				if e != nil || len(data) > shim.OutputChunk {
					return 0, failure("invalid_frame", "invalid or oversized output chunk")
				}
				switch payload.Stream {
				case "stderr":
					if err = writeAll(errout, data); err != nil {
						return 0, err
					}
				case "stdout":
					if len(state.Partial)+len(data) > maxCarryBytes {
						return 0, failure("line_too_long", "stdout carry exceeds bound")
					}
					state.Partial = append(state.Partial, data...)
					if err = drain(); err != nil {
						return 0, err
					}
				default:
					return 0, failure("invalid_frame", "unknown output stream")
				}
			}
			if ev.Kind == "shim.exit" && len(state.Partial) > 0 {
				if err = writeAll(out, state.Partial); err != nil {
					return 0, err
				}
				if afterWrite != nil {
					afterWrite(state.Partial)
				}
				state.Partial = nil
			}
			if ev.Kind == "shim.exit" {
				var exit shim.Exit
				if json.Unmarshal(ev.Payload, &exit) != nil {
					return 0, failure("invalid_frame", "invalid exit payload")
				}
				state.Exit = &exit
			}
			if err = checkpoint(); err != nil {
				return 0, err
			}
			if err = c.Ack(spec.Session, ev.Cursor); err != nil {
				return 0, err
			}
			if ev.Cursor == high {
				if err = emitAttach(); err != nil {
					return 0, err
				}
			}
			if ev.Kind == "shim.exit" {
				if err = emitAttach(); err != nil {
					return 0, err
				}
				return providerExitCode(*state.Exit), nil
			}
		}
	}
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func drainLines(state *Checkpoint, limit int, out io.Writer, commit func() error, afterWrite func([]byte)) error {
	for {
		i := bytes.IndexByte(state.Partial, '\n')
		if i < 0 {
			if len(state.Partial) > limit {
				return failure("line_too_long", "stdout line exceeds 64 MiB")
			}
			return nil
		}
		if i > limit {
			return failure("line_too_long", "stdout line exceeds 64 MiB")
		}
		line := state.Partial[:i+1]
		if err := writeAll(out, line); err != nil {
			return err
		}
		if afterWrite != nil {
			afterWrite(line)
		}
		var init struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal(line, &init) == nil && init.Type == "system" && init.Subtype == "init" {
			state.Init = bytes.Clone(line)
		}
		state.Partial = state.Partial[i+1:]
		if err := commit(); err != nil {
			return err
		}
	}
}

func providerExitCode(exit shim.Exit) int {
	if exit.Signal != 0 {
		return 128 + exit.Signal
	}
	return exit.Status
}
