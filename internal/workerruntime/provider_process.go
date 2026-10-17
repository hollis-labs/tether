package workerruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	gop "github.com/hollis-labs/libs/harness/core/adapters/provider"
	"github.com/hollis-labs/libs/harness/core/adapters/turn"
	"github.com/hollis-labs/libs/llm/core/contracts/runtimes"
	"github.com/hollis-labs/tether/internal/shimagy"
)

// RunProviderProcess keeps provider protocol state inside the shim's process
// group. An outpost reconnect never reinitializes an already-running thread.
func RunProviderProcess(ctx context.Context, path string, input io.Reader, output, stderr io.Writer) error {
	cfg, err := readProviderProcess(path)
	if err != nil {
		return err
	}
	if cfg.Provider == "antigravity" {
		return shimagy.Run(ctx, shimagy.Config{Binary: cfg.Binary, Launch: *cfg.Launch, ExtraArgs: cfg.ExtraArgs}, input, output, stderr)
	}
	if cfg.Provider == "codex" && cfg.Launch.Convention.Mode == runtimes.ModeSubprocessPerTurn {
		return runCodexExec(ctx, cfg, input, output, stderr)
	}
	if cfg.Provider != "codex" || cfg.Launch.Convention.Mode != runtimes.ModeJSONRPCStdio {
		return fmt.Errorf("unsupported worker provider protocol %s/%s", cfg.Provider, cfg.RuntimeKind)
	}
	args, err := cfg.Launch.TurnArgv(gop.TurnInput{}, cfg.ExtraArgs...)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, cfg.Binary, args...) //nolint:gosec // G204: local provider pin and shared native launch projection, no shell.
	cmd.Dir, cmd.Stderr = cfg.Workdir, stderr
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	rpc := &providerRPC{writer: stdin, pending: map[int64]chan rpcReply{}, done: make(chan struct{}), completed: make(chan struct{}, 1)}
	go rpc.read(stdout, output)
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = stdout.Close()
		_ = cmd.Wait()
		<-rpc.done
	}()
	var session turn.CodexAppServerSession
	lines := make(chan []byte)
	scanDone := make(chan error, 1)
	readCtx, stopRead := context.WithCancel(ctx)
	defer stopRead()
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			select {
			case lines <- append([]byte(nil), scanner.Bytes()...):
			case <-readCtx.Done():
				return
			}
		}
		scanDone <- scanner.Err()
	}()
	for {
		var line []byte
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rpc.done:
			return fmt.Errorf("provider protocol ended")
		case err := <-scanDone:
			return err
		case line = <-lines:
		}
		var frame struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &frame) != nil || frame.Type != "user" || frame.Message.Role != "user" {
			return fmt.Errorf("invalid provider user frame")
		}
		callCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := session.SendTurn(callCtx, rpc, frame.Message.Content, turn.CodexAppServerOptions{ClientName: "tether-worker", ClientVersion: "1", CWD: cfg.ProjectDir})
		cancel()
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-rpc.done:
			return fmt.Errorf("provider ended during turn")
		case <-rpc.completed:
		}
	}
}

type rpcReply struct {
	result json.RawMessage
	err    error
}
type providerRPC struct {
	mu        sync.Mutex
	writeMu   sync.Mutex
	writer    io.Writer
	next      int64
	pending   map[int64]chan rpcReply
	done      chan struct{}
	completed chan struct{}
}

func (r *providerRPC) write(v any) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	return json.NewEncoder(r.writer).Encode(v)
}

func (r *providerRPC) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	r.mu.Lock()
	r.next++
	id := r.next
	ch := make(chan rpcReply, 1)
	r.pending[id] = ch
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pending, id); r.mu.Unlock() }()
	if err := r.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.done:
		return nil, fmt.Errorf("provider protocol ended")
	case reply := <-ch:
		if reply.err == nil && method == "initialize" {
			if err := r.write(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
				return nil, err
			}
		}
		return reply.result, reply.err
	}
}

func (r *providerRPC) read(input io.Reader, output io.Writer) {
	defer close(r.done)
	finalText := ""
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			return
		}
		if _, err := fmt.Fprintln(output, string(scanner.Bytes())); err != nil {
			return
		}
		if len(frame.ID) == 0 {
			if frame.Method == "item/completed" {
				var params struct {
					Item struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"item"`
				}
				if json.Unmarshal(frame.Params, &params) == nil && params.Item.Type == "agentMessage" {
					finalText = params.Item.Text
				}
			}
			if frame.Method == "turn/completed" {
				var params struct {
					Turn struct {
						ID     string `json:"id"`
						Status string `json:"status"`
					} `json:"turn"`
				}
				if json.Unmarshal(frame.Params, &params) != nil || params.Turn.ID == "" {
					return
				}
				if err := writeProviderResult(output, "codex", params.Turn.ID, finalText, params.Turn.Status != "completed"); err != nil {
					return
				}
				finalText = ""
				select {
				case r.completed <- struct{}{}:
				default:
				}
			}
			continue
		}
		if frame.Method != "" {
			// No interactive approval channel exists on a worker. The planted
			// permission posture controls execution; unsolicited requests fail.
			if err := r.write(map[string]any{"id": frame.ID, "error": map[string]any{"code": -32601, "message": "worker has no interactive request handler"}}); err != nil {
				return
			}
			continue
		}
		var id int64
		if json.Unmarshal(frame.ID, &id) != nil {
			return
		}
		r.mu.Lock()
		ch := r.pending[id]
		r.mu.Unlock()
		if ch != nil {
			reply := rpcReply{result: frame.Result}
			if len(frame.Error) > 0 && string(frame.Error) != "null" {
				reply.err = fmt.Errorf("provider JSON-RPC request failed: %s", frame.Error)
			}
			select {
			case ch <- reply:
			default:
			}
		}
	}
}
