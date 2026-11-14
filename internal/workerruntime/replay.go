package workerruntime

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hollis-labs/libs/harness/core/shim"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/workerwire"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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

// DeadHostGrace is how old a dead host dir must be before Restore moves it aside: younger ones keep their
// output where an operator would look for it.
const DeadHostGrace = 24 * time.Hour

// deadHostsDir is where Restore moves dead host dirs, under a dated subdirectory. They are moved, never
// deleted, so a pruning mistake is undone with mv.
const deadHostsDir = "hosts-dead"

// errDeadHost marks a host dir with nothing to re-adopt: no session record, no launch descriptor, or no
// shim answering on its control socket. Detecting it costs a few file reads and one refused dial.
type errDeadHost struct {
	reason error
	// noRecord: meta.json is missing or unreadable, so the host pid could not be checked.
	noRecord bool
}

func (e errDeadHost) Error() string { return "dead host: " + e.reason.Error() }

// Restore re-adopts the shims that are still alive after a worker restart. A session whose shim has gone
// (the agent finished and its host exited) or whose state is damaged is skipped and logged: one dead
// session must never keep the worker from starting and serving the live ones.
//
// It runs before the worker reports ready, so it must stay cheap however many dirs have piled up
// (CW-20261114-0056: 309 dead of 344 outran outpost's plugin init deadline). A dir is classified before
// anything costly: only a host whose shim accepts the control connection has its output read. Dead dirs
// older than DeadHostGrace are moved to hosts-dead/<date>/. The counts are kept for the worker hello.
func (e *Executor) Restore() error {
	counts := &workerwire.RestoreCounts{}
	e.restored = counts
	dirs, err := os.ReadDir(filepath.Join(e.Root, "hosts"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	now := time.Now()
	served := &servedLaunches{}
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		path := filepath.Join(e.Root, "hosts", dir.Name())
		err := e.restoreOne(path)
		var dead errDeadHost
		switch {
		case err == nil:
			counts.Live++
		case errors.As(err, &dead) && dead.noRecord && served.has(filepath.Join(path, "launch.json")):
			// No readable record, but a shim-host still serves this dir (a meta.json corrupted or cut short by
			// a crash): not dead, never moved; counted failed, so the hub does not trust the inventory as complete.
			counts.Failed++
			fmt.Fprintf(os.Stderr, "tether-worker: session host %s has a live shim-host but no readable meta.json: %v\n", dir.Name(), dead.reason)
		case errors.As(err, &dead):
			counts.DeadSkipped++
			if moved, mvErr := e.pruneDeadHost(path, now); mvErr != nil {
				fmt.Fprintf(os.Stderr, "tether-worker: dead session host %s not pruned: %v\n", dir.Name(), mvErr)
			} else if moved {
				counts.Pruned++
			}
		default:
			counts.Failed++
			fmt.Fprintf(os.Stderr, "tether-worker: skipping session host %s: %v\n", dir.Name(), err)
		}
	}
	fmt.Fprintf(os.Stderr, "tether-worker: restore: live=%d dead_skipped=%d pruned=%d failed=%d (%s)\n", counts.Live, counts.DeadSkipped, counts.Pruned, counts.Failed, time.Since(now).Round(time.Millisecond))
	return nil
}

// RestoreCounts returns the counts of the last Restore, or nil before one ran.
func (e *Executor) RestoreCounts() *workerwire.RestoreCounts {
	if e.restored == nil {
		return nil
	}
	c := *e.restored
	return &c
}

// pruneDeadHost moves a dead host dir to hosts-dead/<date>/ once it is DeadHostGrace old. A dir whose
// recorded host pid is still alive is kept: its shim may only be slow, and the reaper still tracks it by
// path.
func (e *Executor) pruneDeadHost(path string, now time.Time) (bool, error) {
	age, ok := hostDirAge(path, now)
	if !ok || age < DeadHostGrace {
		return false, nil
	}
	var s execution
	if err := shimhost.ReadPrivateJSON(filepath.Join(path, "meta.json"), 1<<20, &s); err == nil && s.HostPID > 1 && syscall.Kill(s.HostPID, 0) == nil {
		return false, nil
	}
	dest := filepath.Join(e.Root, deadHostsDir, now.UTC().Format("20060102"))
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return false, err
	}
	return true, os.Rename(path, filepath.Join(dest, filepath.Base(path)))
}

// hostDirAge is the time since anything in a host dir last changed.
func hostDirAge(path string, now time.Time) (time.Duration, bool) {
	newest := time.Time{}
	for _, name := range []string{"meta.json", "output.jsonl", "host.log", "."} {
		if fi, err := os.Stat(filepath.Join(path, name)); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
	}
	if newest.IsZero() {
		return 0, false
	}
	return now.Sub(newest), true
}

func (e *Executor) restoreOne(path string) error {
	var s execution
	if err := shimhost.ReadPrivateJSON(filepath.Join(path, "meta.json"), 1<<20, &s); err != nil {
		return errDeadHost{reason: err, noRecord: true}
	}
	spec, err := shim.ReadLaunch(s.Descriptor)
	if err != nil {
		return errDeadHost{reason: err}
	}
	// Connect first: a gone shim refuses at once (or its socket is missing), before any output is read. A
	// failure after the dial (a refused takeover, a stalled handshake) means a host answered: not dead.
	client, err := shim.Connect(filepath.Join(spec.ControlDir, "control.sock"), spec.Secret, s.ID, spec.Instance, "1", "controller", true)
	if err != nil {
		err = fmt.Errorf("restore %s: %w", s.ID, err)
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
			return errDeadHost{reason: err}
		}
		return err
	}
	if err := readOutputs(path, &s); err != nil {
		_ = client.Close()
		return err
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

// readOutputs loads a re-adopted session's journaled output and its cursor.
func readOutputs(path string, s *execution) error {
	file, err := os.Open(filepath.Join(path, "output.jsonl")) //nolint:gosec // G304: path is the worker-owned session directory
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	for scanner.Scan() {
		var out outputRecord
		if err := json.Unmarshal(scanner.Bytes(), &out); err != nil {
			return err
		}
		s.outputs = append(s.outputs, out)
		if out.Frame.Seq > s.LastSeq {
			s.LastSeq = out.Frame.Seq
			s.Cursor = out.Cursor
		}
	}
	return scanner.Err()
}

// servedLaunches is the set of launch descriptors a running shim-host serves, read once (one ps) the first
// time Restore needs it: only for a dir whose meta.json cannot say whether its host is alive.
type servedLaunches struct {
	paths map[string]bool
}

func (l *servedLaunches) has(launch string) bool {
	if l.paths == nil {
		l.paths = map[string]bool{}
		for _, args := range processArgs() {
			if _, rest, ok := strings.Cut(args, "shim-host --launch "); ok {
				l.paths[strings.Fields(rest + " ")[0]] = true
			}
		}
	}
	return l.paths[launch]
}

// processArgs lists every process's command line. A seam for tests.
var processArgs = func() []string {
	out, err := exec.Command("ps", "-axo", "args=").Output()
	if err != nil {
		return nil
	}
	return strings.Split(string(out), "\n")
}
