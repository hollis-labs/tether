package workerruntime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/hollis-labs/substrate/harness/sandbox"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/workerwire"
	"github.com/hollis-labs/tether/internal/workspace"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type execution struct {
	mu         sync.Mutex
	ID         string `json:"id"`
	Descriptor string `json:"descriptor"`
	Workspace  string `json:"workspace"`
	PID        int    `json:"pid"`
	State      string `json:"state"`
	Sandboxed  bool   `json:"sandboxed"`
	LastSeq    int64  `json:"last_seq"`
	Cursor     string `json:"cursor"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Provider   string `json:"provider"`
	Carry      []byte `json:"carry,omitempty"`
	client     *shim.Client
	outputs    []outputRecord
	pending    map[string]chan shim.Frame
}
type Executor struct {
	Root     string
	Binary   string
	Send     func(workerwire.Frame) error
	mu       sync.Mutex
	sessions map[string]*execution
	Spawn    func(string) (*exec.Cmd, error)
}

func NewExecutor(root, binary string, send func(workerwire.Frame) error) *Executor {
	return &Executor{Root: root, Binary: binary, Send: send, sessions: map[string]*execution{}}
}
func (e *Executor) Inventory() []workerwire.Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []workerwire.Session
	for _, s := range e.sessions {
		s.mu.Lock()
		out = append(out, workerwire.Session{ID: s.ID, State: s.State, LastSeq: s.LastSeq, ExitCode: s.ExitCode})
		s.mu.Unlock()
	}
	return out
}
func (e *Executor) get(id string) *execution { e.mu.Lock(); defer e.mu.Unlock(); return e.sessions[id] }
func (e *Executor) Command(ctx context.Context, f workerwire.Frame) error {
	switch f.T {
	case "launch":
		var spec workerwire.LaunchSpec
		if err := json.Unmarshal(f.Spec, &spec); err != nil {
			return err
		}
		if spec.Session != f.Session {
			return fmt.Errorf("session mismatch")
		}
		s, err := e.launch(ctx, spec)
		if err != nil {
			return err
		}
		return e.Send(workerwire.Frame{T: "launched", CmdID: f.CmdID, Session: f.Session, PID: s.PID, Workspace: s.Workspace, Sandboxed: s.Sandboxed})
	case "stop":
		s := e.get(f.Session)
		if s == nil {
			return fmt.Errorf("unknown execution")
		}
		if err := e.submit(ctx, s, f.CmdID, "control", map[string]any{"action": "kill", "expected_generation": "1"}); err != nil {
			return err
		}
		return e.Send(workerwire.Frame{T: "state", CmdID: f.CmdID, Session: f.Session, State: "exited"})
	case "input":
		s := e.get(f.Session)
		if s == nil {
			return fmt.Errorf("unknown execution")
		}
		spec, err := shim.ReadLaunch(s.Descriptor)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.State = "running"
		_ = e.persist(s)
		s.mu.Unlock()
		_ = e.Send(workerwire.Frame{T: "state", Session: f.Session, State: "running"})
		if err = e.submit(ctx, s, f.CmdID, "inject", shim.Inject{Key: f.CmdID, Actor: spec.Actor, Subject: spec.Subject, Mode: "input", Delivery: "immediate", Data: base64.StdEncoding.EncodeToString(f.Data), Generation: "1"}); err != nil {
			return err
		}
		return e.Send(workerwire.Frame{T: "input_ack", CmdID: f.CmdID, Session: f.Session})
	case "resize":
		return fmt.Errorf("stdio shim does not support resize")
	case "welcome":
		for _, id := range f.Kill {
			if s := e.get(id); s != nil {
				_ = s.client.Send(id, "control", map[string]any{"action": "kill", "expected_generation": "1"})
			}
		}
		for _, cursor := range f.Resume {
			if err := e.replay(cursor.Session, cursor.FromSeq); err != nil {
				return err
			}
		}
		return nil
	case "replay":
		return e.replay(f.Session, f.FromSeq)
	case "ack":
		return nil
	default:
		return fmt.Errorf("unsupported worker command %s", f.T)
	}
}
func (e *Executor) launch(ctx context.Context, spec workerwire.LaunchSpec) (*execution, error) {
	if held := e.get(spec.Session); held != nil {
		return held, nil
	}
	if spec.Session == "" || filepath.Base(spec.Session) != spec.Session {
		return nil, fmt.Errorf("invalid session")
	}
	binary, err := exec.LookPath(spec.Provider)
	if err != nil {
		return nil, err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	ws, err := workspace.Create(filepath.Join(e.Root, "workspaces"), spec.Session, &launch.Plan{BootPrompt: spec.BootPrompt})
	if err != nil {
		return nil, err
	}
	cwd := filepath.Join(ws.Root, "workroot")
	if spec.Workspace.Kind == "git" {
		if spec.Workspace.URL == "" {
			return nil, fmt.Errorf("git workspace URL required")
		}
		if out, err := exec.CommandContext(ctx, "git", "clone", "--", spec.Workspace.URL, cwd).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("workspace clone: %w: %s", err, out)
		}
		if spec.Workspace.Ref != "" {
			if err := exec.CommandContext(ctx, "git", "-C", cwd, "checkout", spec.Workspace.Ref).Run(); err != nil {
				return nil, err
			}
		}
	} else {
		if err := os.MkdirAll(cwd, 0700); err != nil {
			return nil, err
		}
	}
	if spec.Workspace.Subdir != "" {
		clean := filepath.Clean(spec.Workspace.Subdir)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return nil, fmt.Errorf("invalid workspace subdir")
		}
		cwd = filepath.Join(cwd, clean)
	}
	for path, content := range spec.BootFiles {
		if err := plant(cwd, path, []byte(content)); err != nil {
			return nil, err
		}
	}
	for _, f := range spec.NativeFiles {
		if err := plant(cwd, f.RelPath, []byte(f.Content)); err != nil {
			return nil, err
		}
	}
	args := append([]string(nil), spec.Args...)
	if spec.Provider == "claude" {
		var keep []string
		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "--mcp-config", "--append-system-prompt-file", "--system-prompt-file", "--input-format", "--output-format":
				i++
			case "--add-dir":
				i++
			case "-p", "--print", "--verbose", "--include-partial-messages":
			default:
				keep = append(keep, args[i])
			}
		}
		args = append(keep, "--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages")
		if err := e.plantMCP(cwd); err != nil {
			return nil, err
		}
		args = append(args, "--strict-mcp-config", "--mcp-config", filepath.Join(cwd, ".mcp.json"))
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = cwd
	cmd.Env = provider.BuildEnv("merge", nil, nil, spec.Env, os.Environ())
	cmd.Env = append(cmd.Env, "TETHER_WORKER_SESSION="+spec.Session, "TETHER_AGENT_URN="+spec.Actor)
	sandboxed := false
	if spec.Sandbox != nil {
		if _, err := sandbox.Apply(cmd, *spec.Sandbox, cwd); err == nil {
			sandboxed = true
		}
	}
	sum := sha256.Sum256([]byte(spec.Session))
	hostRoot := filepath.Join(e.Root, "hosts", hex.EncodeToString(sum[:8]))
	if err := shimhost.PrivateDir(hostRoot); err != nil {
		return nil, err
	}
	pin := filepath.Join(hostRoot, "pin")
	if err := os.WriteFile(pin, nil, 0600); err != nil {
		return nil, err
	}
	secret, _ := identityToken()
	actor := spec.Actor
	if actor == "" {
		actor = "urn:agent:worker"
	}
	descriptor := filepath.Join(hostRoot, "launch.json")
	launchSpec := shim.Launch{Session: spec.Session, Instance: "urn:instance:worker", Generation: 1, Actor: mesh.Actor{URN: mesh.URN(actor), Kind: mesh.ActorAgent}, Subject: mesh.URN("urn:session:" + spec.Session), Argv: cmd.Args, Env: cmd.Env, Cwd: cmd.Dir, ControlDir: filepath.Join(hostRoot, "c"), JournalDir: filepath.Join(hostRoot, "j"), Secret: secret, PinPath: pin, PinKey: spec.Session, BootGeneration: uuid.NewString(), Reservation: spec.Session, JournalBytes: 256 << 20, Heartbeat: time.Second}
	if err := shimhost.WritePrivateJSON(descriptor, launchSpec); err != nil {
		return nil, err
	}
	var child *exec.Cmd
	if e.Spawn != nil {
		child, err = e.Spawn(descriptor)
	} else {
		child = exec.Command(e.Binary, "shim-host", "--launch", descriptor)
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		logFile, openErr := os.OpenFile(filepath.Join(hostRoot, "host.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if openErr != nil {
			return nil, openErr
		}
		child.Stdout = logFile
		child.Stderr = logFile
		err = child.Start()
		_ = logFile.Close()
	}
	if err != nil {
		return nil, err
	}
	if child != nil {
		go func() { _ = child.Wait() }()
	}
	var client *shim.Client
	deadline := time.Now().Add(10 * time.Second)
	for {
		client, err = shim.Connect(filepath.Join(launchSpec.ControlDir, "control.sock"), secret, spec.Session, launchSpec.Instance, "1", "controller", false)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("shim did not start: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	s := &execution{ID: spec.Session, Descriptor: descriptor, Workspace: cwd, State: "running", Sandboxed: sandboxed, client: client, Provider: spec.Provider, pending: make(map[string]chan shim.Frame)}
	if child != nil {
		s.PID = child.Process.Pid
	}
	if err := e.persist(s); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.sessions[spec.Session] = s
	e.mu.Unlock()
	go e.follow(s)
	_ = client.Replay(spec.Session, "")
	if spec.BootPrompt != "" {
		if spec.Provider == "claude" {
			spec.BootPrompt += "\n\nYou run on a Tether execution outpost. Your canonical hub session is " + spec.Session + " and hub agent address is " + spec.Actor + ". The planted torque, tesseract and tether MCP clients go to the hub. For self-discovery call tether_whoami with as set to that hub agent address. Do not start a local Tether daemon or create a local authority."
		}
		data := []byte(spec.BootPrompt + "\n")
		if spec.Provider == "claude" {
			data, _ = json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": spec.BootPrompt}})
			data = append(data, '\n')
		}
		_ = client.Send(spec.Session, "inject", shim.Inject{Key: "boot:" + spec.Session, Actor: launchSpec.Actor, Subject: launchSpec.Subject, Mode: "input", Delivery: "immediate", Data: base64.StdEncoding.EncodeToString(data), Generation: "1"})
	}
	return s, nil
}
func plant(root, path string, b []byte) error {
	clean := filepath.Clean(path)
	if path == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("invalid boot file path")
	}
	target := filepath.Join(root, clean)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	return os.WriteFile(target, b, 0600)
}
func identityToken() (string, error) { return uuid.NewString() + uuid.NewString(), nil }
func (e *Executor) follow(s *execution) {
	for f := range s.client.Frames {
		if f.ReplyTo != "" {
			s.mu.Lock()
			ch := s.pending[f.ReplyTo]
			s.mu.Unlock()
			if ch != nil {
				select {
				case ch <- f:
				default:
				}
			}
		}
		if f.Type != "event" {
			continue
		}
		var body struct {
			Event mesh.Event `json:"event"`
		}
		if json.Unmarshal(f.Body, &body) != nil {
			continue
		}
		event := body.Event
		s.mu.Lock()
		s.Cursor = event.Cursor
		if event.Kind == "shim.started" {
			var b struct {
				PID int `json:"pid"`
			}
			_ = json.Unmarshal(event.Payload, &b)
			s.PID = b.PID
		}
		if event.Kind == "shim.output" {
			var chunk struct {
				Data   string `json:"data"`
				Stream string `json:"stream"`
			}
			if json.Unmarshal(event.Payload, &chunk) == nil {
				data, err := base64.StdEncoding.DecodeString(chunk.Data)
				if err == nil {
					s.LastSeq++
					out := outputRecord{Frame: workerwire.Frame{T: "output", Session: s.ID, Seq: s.LastSeq, Data: data, Stream: chunk.Stream, At: event.Time.UTC().Format(time.RFC3339Nano)}, Cursor: event.Cursor}
					if err := e.appendOutput(s, out); err != nil {
						s.mu.Unlock()
						continue
					}
					s.outputs = append(s.outputs, out)
					if chunk.Stream == "stdout" {
						s.Carry = append(s.Carry, data...)
						for {
							at := strings.IndexByte(string(s.Carry), '\n')
							if at < 0 {
								break
							}
							line := s.Carry[:at]
							s.Carry = s.Carry[at+1:]
							var result struct {
								Type string `json:"type"`
							}
							if s.Provider != "claude" || json.Unmarshal(line, &result) == nil && result.Type == "result" {
								s.State = "idle"
								_ = e.Send(workerwire.Frame{T: "state", Session: s.ID, State: "idle"})
							}
						}
					}
					_ = e.persist(s)
					_ = e.Send(out.Frame)
				}
			}
		}
		if event.Kind == "shim.exit" {
			var exit shim.Exit
			_ = json.Unmarshal(event.Payload, &exit)
			s.State = "exited"
			s.ExitCode = &exit.Status
			_ = e.Send(workerwire.Frame{T: "state", Session: s.ID, State: "exited", ExitCode: &exit.Status})
		}
		_ = e.persist(s)
		s.mu.Unlock()
	}
}
