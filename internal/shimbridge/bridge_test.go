//go:build linux

package shimbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/shimhost"
	"golang.org/x/sys/unix"
)

type bridgeConfig struct {
	Launch         shim.Launch
	StatePath      string
	PauseAfterLine string
}
type bridgeState = Checkpoint

func readState(path string) (Checkpoint, error) {
	s, e := ReadCheckpoint(path)
	if os.IsNotExist(e) {
		return s, nil
	}
	return s, e
}
func saveJSON(path string, v any) error { return shimhost.WritePrivateJSON(path, v) }

// The helper is the compiled test binary. No model CLI is ever discovered.
func TestProviderProcess(t *testing.T) {
	mode := os.Getenv("SHIM_TEST_PROCESS")
	if mode == "" {
		return
	}
	parent := os.Getppid()
	if parent <= 1 || unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0) != nil || os.Getppid() != parent {
		os.Exit(96)
	}
	b, e := os.ReadFile(os.Getenv("SHIM_TEST_CONFIG"))
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
		descriptor := filepath.Join(filepath.Dir(cfg.StatePath), "launch.json")
		if e = saveJSON(descriptor, cfg.Launch); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(90)
		}
		code, e := run(context.Background(), Options{DescriptorPath: descriptor, StatePath: cfg.StatePath, Attach: attach, OnAttach: func(ev AttachEvent) error { return json.NewEncoder(os.Stderr).Encode(ev) }}, os.Stdin, os.Stdout, os.Stderr, func(line []byte) {
			if cfg.PauseAfterLine != "" && bytes.Contains(line, []byte(cfg.PauseAfterLine)) {
				_ = os.WriteFile(filepath.Join(filepath.Dir(cfg.StatePath), "after-write"), nil, 0600)
				for {
					time.Sleep(time.Hour)
				}
			}
		})
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(93)
		}
		os.Exit(code)
	}
	os.Exit(94)
}
func fakeClaude() {
	if err := publishFixtureChildPID(filepath.Join(os.Getenv("HOME"), "child.pid"), os.Getpid()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(90)
	}
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

// Publish only a complete PID witness; readers must never see a partial integer.
func publishFixtureChildPID(path string, pid int) error {
	pending := path + ".pending"
	defer func() { _ = os.Remove(pending) }()
	if err := os.WriteFile(pending, []byte(strconv.Itoa(pid)), 0600); err != nil {
		return err
	}
	return os.Rename(pending, path)
}
