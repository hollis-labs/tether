//go:build linux

// Package shimbridge is a test-only G1 spike, not a production bridge.
package shimbridge

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
)

type bridgeConfig struct {
	Launch         shim.Launch
	StatePath      string
	ReplayInit     bool
	PauseAfterLine string
}
type bridgeState struct {
	Cursor  string
	Journal string
	Counter uint64
	// A cursor can land mid-line. Carry the un-emitted suffix durably with it.
	Partial []byte
	Init    []byte
}
type stateStore struct {
	mu    sync.Mutex
	path  string
	value bridgeState
}

func readState(path string) (bridgeState, error) {
	var s bridgeState
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return s, nil
	}
	if e != nil {
		return s, e
	}
	e = json.Unmarshal(b, &s)
	return s, e
}
func saveJSON(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(path+".new", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(path+".new", path); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	e = d.Sync()
	ce = d.Close()
	if e == nil {
		e = ce
	}
	return e
}
func (s *stateStore) update(fn func(*bridgeState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.value)
	return saveJSON(s.path, s.value)
}
func (s *stateStore) next() (string, error) {
	var n uint64
	e := s.update(func(v *bridgeState) { v.Counter++; n = v.Counter })
	return "bridge-" + strconv.FormatUint(n, 10), e
}

// The helper is the compiled test binary. No model CLI is ever discovered.
func TestSpikeProcess(t *testing.T) {
	mode := os.Getenv("G1_PROCESS")
	if mode == "" {
		return
	}
	b, e := os.ReadFile(os.Getenv("G1_CONFIG"))
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(90)
	}
	var cfg bridgeConfig
	if e = json.Unmarshal(b, &cfg); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(90)
	}
	switch mode {
	case "host":
		h, e := shim.Start(cfg.Launch)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(91)
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		e = h.Close()
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(92)
		}
		os.Exit(0)
	case "child":
		fakeClaude()
		os.Exit(0)
	case "bridge":
		attach := false
		for _, a := range os.Args {
			if a == "--attach" {
				attach = true
			}
		}
		code, e := runBridge(cfg, attach, os.Stdin, os.Stdout, os.Stderr)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(93)
		}
		os.Exit(code)
	}
	os.Exit(94)
}
func fakeClaude() {
	_ = os.WriteFile(filepath.Join(os.Getenv("HOME"), "child.pid"), []byte(strconv.Itoa(os.Getpid())), 0600)
	emit := func(v any) { b, _ := json.Marshal(v); fmt.Fprintln(os.Stdout, string(b)) }
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": "fake-native"})
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), 2<<20)
	for scan.Scan() {
		var f struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Message   struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scan.Bytes(), &f) != nil {
			os.Exit(4)
		}
		if f.Type == "control_request" {
			emit(map[string]any{"type": "control_response", "response": map[string]string{"subtype": "success", "request_id": f.RequestID}})
			emit(map[string]any{"type": "result", "subtype": "success", "session_id": "fake-native", "result": "interrupted"})
			continue
		}
		text := f.Message.Content
		switch text {
		case "exit":
			os.Exit(7)
		case "exit-final":
			_, _ = io.WriteString(os.Stdout, `{"type":"assistant","message":{"content":[{"type":"text","text":"final-no-newline"}]}}`)
			os.Exit(7)
		case "burst":
			for i := 0; i < 90; i++ {
				_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), shim.OutputChunk))
			}
			continue
		case "block-input":
			emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]string{"type": "text", "text": "blocked-input"}}}})
			select {}
		case "partial":
			_, _ = io.WriteString(os.Stdout, `{"type":"assistant","message":{"content":[{"type":"text","text":"split`)
			continue
		case "finish-partial":
			_, _ = io.WriteString(os.Stdout, "-tail\"}]}}\n")
		default:
			fmt.Fprintln(os.Stderr, "stderr:"+text)
			emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]string{"type": "text", "text": "reply:" + text}}}})
		}
		if text != "hold" {
			emit(map[string]any{"type": "result", "subtype": "success", "session_id": "fake-native", "result": "done:" + text})
		}
	}
}

// One outstanding input round trip, correlated by request ID. An unknown or
// failed effect terminates the bridge; nothing resends that effect.
func runBridge(cfg bridgeConfig, attach bool, in io.Reader, out, errout io.Writer) (int, error) {
	state, e := readState(cfg.StatePath)
	if e != nil {
		return 0, e
	}
	c, e := shim.Connect(filepath.Join(cfg.Launch.ControlDir, "control.sock"), cfg.Launch.Secret, cfg.Launch.Session, cfg.Launch.Instance, "1", "controller", false)
	if e != nil {
		return 0, e
	}
	defer c.Close()
	if state.Journal != "" && state.Journal != c.Journal {
		return 0, fmt.Errorf("journal mismatch")
	}
	store := &stateStore{path: cfg.StatePath, value: state}
	if e = store.update(func(v *bridgeState) { v.Journal = c.Journal }); e != nil {
		return 0, e
	}
	if attach && cfg.ReplayInit && len(state.Init) > 0 {
		if e = writeAll(out, state.Init); e != nil {
			return 0, e
		}
	}
	if e = c.Replay(cfg.Launch.Session, state.Cursor); e != nil {
		return 0, e
	}
	replies := make(chan shim.Frame, 1)
	inputErr := make(chan error, 1)
	go func() {
		b := make([]byte, shim.OutputChunk)
		for {
			n, e := in.Read(b)
			if n > 0 {
				key, ke := store.next()
				if ke != nil {
					inputErr <- ke
					return
				}
				inj := shim.Inject{Key: key, Actor: cfg.Launch.Actor, Subject: cfg.Launch.Subject, Mode: "input", Delivery: "immediate", Generation: "1", Data: base64.StdEncoding.EncodeToString(b[:n])}
				raw, _ := json.Marshal(inj)
				if ke = c.SendFrame(shim.Frame{Major: 1, Type: "inject", RequestID: key, Session: cfg.Launch.Session, Epoch: c.Epoch, Body: raw}); ke != nil {
					inputErr <- ke
					return
				}
				f, ok := <-replies
				if !ok {
					return
				}
				var r struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(f.Body, &r) != nil || f.Type == "error" || r.Code != "bytes_written" {
					inputErr <- fmt.Errorf("inject failed: %s", f.Body)
					return
				}
			}
			if e != nil {
				if e != io.EOF {
					inputErr <- e
				} else {
					inputErr <- io.EOF
				}
				return
			}
		}
	}()
	defer close(replies)
	for {
		select {
		case e := <-inputErr:
			// EOF detaches the controller. Shim v0 cannot close hosted stdin.
			if e == io.EOF {
				return 0, nil
			}
			return 0, e
		case f, ok := <-c.Frames:
			if !ok {
				return 0, fmt.Errorf("shim disconnected")
			}
			if f.ReplyTo != "" && len(f.ReplyTo) > 7 && f.ReplyTo[:7] == "bridge-" {
				replies <- f
				continue
			}
			if f.Type == "error" {
				return 0, fmt.Errorf("shim error: %s", f.Body)
			}
			if f.Type != "event" {
				continue
			}
			var body struct {
				Event mesh.Event `json:"event"`
			}
			if e = json.Unmarshal(f.Body, &body); e != nil {
				return 0, e
			}
			ev := body.Event
			if ev.Kind == "shim.output_gap" {
				return 0, fmt.Errorf("shim output gap: %s", ev.Payload)
			}
			if ev.Kind == "shim.output" {
				var p struct {
					Stream string `json:"stream"`
					Data   string `json:"data"`
				}
				if e = json.Unmarshal(ev.Payload, &p); e != nil {
					return 0, e
				}
				data, de := base64.StdEncoding.DecodeString(p.Data)
				if de != nil {
					return 0, de
				}
				if p.Stream == "stderr" {
					if e = writeAll(errout, data); e != nil {
						return 0, e
					}
				} else {
					state.Partial = append(state.Partial, data...)
					for {
						i := bytes.IndexByte(state.Partial, '\n')
						if i < 0 {
							break
						}
						line := state.Partial[:i+1]
						if e = writeAll(out, line); e != nil {
							return 0, e
						}
						if cfg.PauseAfterLine != "" && bytes.Contains(line, []byte(cfg.PauseAfterLine)) {
							if e = os.WriteFile(filepath.Join(filepath.Dir(cfg.StatePath), "after-write"), []byte(ev.Cursor), 0600); e != nil {
								return 0, e
							}
							for {
								time.Sleep(time.Hour)
							} // deterministic crash after pipe write, before cursor persistence
						}
						var init struct {
							Type    string `json:"type"`
							Subtype string `json:"subtype"`
						}
						if json.Unmarshal(line, &init) == nil && init.Type == "system" && init.Subtype == "init" {
							state.Init = append([]byte(nil), line...)
						}
						state.Partial = state.Partial[i+1:]
					}
				}
			}
			if ev.Kind == "shim.exit" && len(state.Partial) > 0 {
				if e = writeAll(out, state.Partial); e != nil {
					return 0, e
				}
				state.Partial = nil
			}
			// Persist cursor and partial together after writes. This still cannot
			// prove agentkit consumed its pipe buffer (the design's D4 weakness).
			if e = store.update(func(v *bridgeState) {
				v.Cursor = ev.Cursor
				v.Partial = append([]byte(nil), state.Partial...)
				v.Init = append([]byte(nil), state.Init...)
			}); e != nil {
				return 0, e
			}
			if e = c.Ack(cfg.Launch.Session, ev.Cursor); e != nil {
				return 0, e
			}
			if ev.Kind == "shim.exit" {
				var exit shim.Exit
				if e = json.Unmarshal(ev.Payload, &exit); e != nil {
					return 0, e
				}
				if exit.Signal != 0 {
					return 128 + exit.Signal, nil
				}
				return exit.Status, nil
			}
		}
	}
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
