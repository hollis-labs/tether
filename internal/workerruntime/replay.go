package workerruntime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/workerwire"
	"os"
	"path/filepath"
)

type outputRecord struct {
	Frame  workerwire.Frame `json:"frame"`
	Cursor string           `json:"cursor"`
}

func (e *Executor) persist(s *execution) error {
	return shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(s.Descriptor), "meta.json"), s)
}
func (e *Executor) appendOutput(s *execution, out outputRecord) error {
	file, err := os.OpenFile(filepath.Join(filepath.Dir(s.Descriptor), "output.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err = json.NewEncoder(file).Encode(out); err != nil {
		return err
	}
	return file.Sync()
}
func (e *Executor) replay(id string, from int64) error {
	s := e.get(id)
	if s == nil {
		return fmt.Errorf("unknown execution")
	}
	s.mu.Lock()
	var batch []workerwire.Frame
	bytes := 0
	next := int64(0)
	for _, out := range s.outputs {
		if out.Frame.Seq >= from {
			if len(batch) >= 256 || (len(batch) > 0 && bytes+len(out.Frame.Data) > 1<<20) {
				next = out.Frame.Seq
				break
			}
			batch = append(batch, out.Frame)
			bytes += len(out.Frame.Data)
		}
	}
	state := workerwire.Frame{T: "state", Session: s.ID, State: s.State, ExitCode: s.ExitCode}
	s.mu.Unlock()
	for _, f := range batch {
		if err := e.Send(f); err != nil {
			return err
		}
	}
	if next > 0 {
		return e.Send(workerwire.Frame{T: "replay_end", Session: id, FromSeq: next})
	}
	return e.Send(state)
}

// Restore re-adopts the shims that are still alive after a worker restart. A session whose shim has gone
// (the agent finished and its host exited) or whose state is damaged is skipped and logged: one dead
// session must never keep the worker from starting and serving the live ones.
func (e *Executor) Restore() error {
	dirs, err := os.ReadDir(filepath.Join(e.Root, "hosts"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		if err := e.restoreOne(filepath.Join(e.Root, "hosts", dir.Name())); err != nil {
			fmt.Fprintf(os.Stderr, "tether-worker: skipping session host %s: %v\n", dir.Name(), err)
		}
	}
	return nil
}
func (e *Executor) restoreOne(path string) error {
	var s execution
	if err := shimhost.ReadPrivateJSON(filepath.Join(path, "meta.json"), 1<<20, &s); err != nil {
		return err
	}
	spec, err := shim.ReadLaunch(s.Descriptor)
	if err != nil {
		return err
	}
	if file, err := os.Open(filepath.Join(path, "output.jsonl")); err == nil { //nolint:gosec // G304: path is the worker-owned session directory
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64<<10), 2<<20)
		for scanner.Scan() {
			var out outputRecord
			if err := json.Unmarshal(scanner.Bytes(), &out); err != nil {
				_ = file.Close()
				return err
			}
			s.outputs = append(s.outputs, out)
			if out.Frame.Seq > s.LastSeq {
				s.LastSeq = out.Frame.Seq
				s.Cursor = out.Cursor
			}
		}
		err = scanner.Err()
		_ = file.Close()
		if err != nil {
			return err
		}
	}
	client, err := shim.Connect(filepath.Join(spec.ControlDir, "control.sock"), spec.Secret, s.ID, spec.Instance, "1", "controller", true)
	if err != nil {
		return fmt.Errorf("restore %s: %w", s.ID, err)
	}
	s.client = client
	s.pending = make(map[string]chan shim.Frame)
	e.sessions[s.ID] = &s
	if s.State != "exited" && len(s.AgentTokens) > 0 {
		// Resume refreshing: the files persist across an outpost restart.
		if _, stop, err := e.startAgentTokens(s.ID, s.AuthDir, s.AgentTokens); err == nil {
			s.stopTokens = stop
		} else {
			fmt.Fprintf(os.Stderr, "tether-worker: session %s agent tokens not resumed: %v\n", s.ID, err)
		}
	}
	go e.follow(&s)
	return client.Replay(s.ID, s.Cursor)
}
