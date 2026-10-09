package app

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	localdaemon "github.com/hollis-labs/libs/util/localdaemon"
)

// processInspector is what the daemon-start sweep asks the OS about a pid
// (CW-20260912-0085). A pid alone does not identify a process: pids are
// reused, so a session's recorded pid may now name something unrelated.
// The start time does: the same pid with the same start time is the same
// process.
type processInspector interface {
	// alive reports whether a process with pid exists.
	alive(pid int) bool
	// startTime returns when pid started, as RFC3339 UTC at second
	// precision, or false when that cannot be read.
	startTime(pid int) (string, bool)
	// command returns pid's command line, or false when it cannot be read.
	command(pid int) (string, bool)
}

// processProbeTimeout bounds one ps call.
const processProbeTimeout = 2 * time.Second

// osProcessInspector reads process facts with ps, whose `-o lstart=` and
// `-o command=` columns mean the same on Linux (procps) and macOS. TZ and
// LC_ALL are pinned so lstart is UTC in a fixed format.
type osProcessInspector struct{}

func (osProcessInspector) alive(pid int) bool { return localdaemon.IsAlive(pid) }

func (osProcessInspector) startTime(pid int) (string, bool) {
	out, ok := runPS(pid, "lstart=")
	if !ok {
		return "", false
	}
	// lstart pads a single-digit day with a second space; collapse runs of
	// spaces so one layout fits both.
	t, err := time.Parse("Mon Jan _2 15:04:05 2006", strings.Join(strings.Fields(out), " "))
	if err != nil {
		return "", false
	}
	return t.UTC().Format(time.RFC3339), true
}

func (osProcessInspector) command(pid int) (string, bool) {
	return runPS(pid, "command=")
}

func runPS(pid int, column string) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), processProbeTimeout)
	defer cancel()
	// #nosec G204 -- fixed ps arguments; pid is formatted from an int.
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", column)
	cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(out))
	return s, s != ""
}
