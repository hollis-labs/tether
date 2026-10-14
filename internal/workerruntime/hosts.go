package workerruntime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// OrphanGrace is how long a shim-host may sit with no live agent before the reaper terminates it. It covers
// the window between an agent exiting and the normal teardown (or a worker restart re-adopting the host).
const OrphanGrace = 10 * time.Minute

// HostReapInterval is how often the worker re-scans its host directories.
const HostReapInterval = time.Minute

// pgrep is a seam: the host pid is found from its command line because meta.json only records it for hosts
// this worker spawned itself, and a host adopted across worker restarts may have been reparented.
var pgrep = func(args ...string) []int {
	out, err := exec.Command("pgrep", args...).Output() //nolint:gosec // G204: fixed binary, arguments are a descriptor path this worker owns or a numeric pid
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if n, err := strconv.Atoi(f); err == nil && n > 1 {
			pids = append(pids, n)
		}
	}
	return pids
}

// ScanHosts lists every shim-host directory with its session, state, age and whether the host process and
// its agent child are alive. A host is orphaned when it is alive, has no live child, and this worker is not
// serving a running session on it. Dirs without a live host are listed (HostAlive false) but never orphaned:
// they hold replayable output, not a process.
func (e *Executor) ScanHosts() []workerwire.Host {
	dirs, err := os.ReadDir(filepath.Join(e.Root, "hosts"))
	if err != nil {
		return nil
	}
	var out []workerwire.Host
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		path := filepath.Join(e.Root, "hosts", d.Name())
		h := workerwire.Host{Dir: d.Name()}
		var s execution
		if err := shimhost.ReadPrivateJSON(filepath.Join(path, "meta.json"), 1<<20, &s); err == nil {
			h.Session, h.State = s.ID, s.State
		}
		if live := e.get(h.Session); live != nil && h.Session != "" {
			live.mu.Lock()
			h.State = live.State
			live.mu.Unlock()
		}
		newest := time.Time{}
		for _, name := range []string{"meta.json", "output.jsonl", "host.log", "."} {
			if fi, err := os.Stat(filepath.Join(path, name)); err == nil && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		if !newest.IsZero() {
			h.AgeSec = int64(time.Since(newest).Seconds())
		}
		if pids := pgrep("-f", "shim-host --launch "+filepath.Join(path, "launch.json")); len(pids) > 0 {
			h.HostPID, h.HostAlive = pids[0], true
			h.ChildAlive = len(pgrep("-P", strconv.Itoa(h.HostPID))) > 0
		}
		h.Orphaned = h.HostAlive && !h.ChildAlive && h.State != "running" && h.State != "idle" && h.State != "launching"
		if h.State == "" && h.HostAlive && !h.ChildAlive {
			h.Orphaned = true
		}
		out = append(out, h)
	}
	return out
}

// ReapOrphans terminates orphaned shim-hosts at least grace old and removes their runtime files (meta.json
// and output.jsonl stay, as for a normal teardown, so final output can still replay). It returns the dir
// names reaped. A session this worker still holds as exited is dropped from the inventory too.
func (e *Executor) ReapOrphans(grace time.Duration) []string {
	var reaped []string
	for _, h := range e.ScanHosts() {
		if !h.Orphaned || time.Duration(h.AgeSec)*time.Second < grace {
			continue
		}
		teardownHost(filepath.Join(e.Root, "hosts", h.Dir, "launch.json"), h.HostPID)
		reaped = append(reaped, h.Dir)
	}
	return reaped
}

// ReapLoop reaps on startup and every interval, reporting the host list to the hub after each pass.
func (e *Executor) ReapLoop(ctx context.Context, interval, grace time.Duration) {
	for {
		_ = e.ReapOrphans(grace)
		_ = e.Send(workerwire.Frame{T: "hosts", Hosts: e.ScanHosts()})
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
