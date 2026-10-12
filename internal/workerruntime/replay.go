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
	defer s.mu.Unlock()
	for _, out := range s.outputs {
		if out.Frame.Seq >= from {
			if err := e.Send(out.Frame); err != nil {
				return err
			}
		}
	}
	return e.Send(workerwire.Frame{T: "state", Session: s.ID, State: s.State, ExitCode: s.ExitCode})
}
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
		path := filepath.Join(e.Root, "hosts", dir.Name())
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
					file.Close()
					return err
				}
				s.outputs = append(s.outputs, out)
				if out.Frame.Seq > s.LastSeq {
					s.LastSeq = out.Frame.Seq
					s.Cursor = out.Cursor
				}
			}
			err = scanner.Err()
			file.Close()
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
		go e.follow(&s)
		if err := client.Replay(s.ID, s.Cursor); err != nil {
			return err
		}
	}
	return nil
}
