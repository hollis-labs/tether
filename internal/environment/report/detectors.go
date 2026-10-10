package report

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type Detectors struct {
	ExecCommand func(ctx context.Context, name string, arg ...string) ([]byte, error)
	LookPath    func(file string) (string, error)
	Stat        func(name string) (os.FileInfo, error)
	Statfs      func(path string, buf *unix.Statfs_t) error
	ReadFile    func(string) ([]byte, error)
}

func DefaultDetectors() *Detectors {
	return &Detectors{
		ExecCommand: func(ctx context.Context, name string, arg ...string) ([]byte, error) {
			probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			//nolint:gosec // bounded local detector commands, never a model turn
			cmd := exec.CommandContext(probeCtx, name, arg...)
			cmd.WaitDelay = 200 * time.Millisecond
			out := &probeOutput{}
			cmd.Stdout, cmd.Stderr = out, out
			err := cmd.Run()
			return out.buffer.Bytes(), err
		},
		LookPath: exec.LookPath,
		Stat:     os.Stat,
		Statfs:   unix.Statfs,
		ReadFile: os.ReadFile,
	}
}

// Keep draining output after the limit so a noisy probe cannot block on a full
// pipe or allocate unbounded memory inside the daemon.
type probeOutput struct{ buffer bytes.Buffer }

func (o *probeOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 32*1024 - o.buffer.Len(); remaining > 0 {
		_, _ = o.buffer.Write(p[:min(n, remaining)])
	}
	return n, nil
}

type SandboxProtectData struct {
	Enabled         bool
	BwrapUsable     bool
	BwrapChecked    bool
	PlanUnavailable bool
	Codex           string
}

func (d *Detectors) DetectProviders(ctx context.Context, sandboxData SandboxProtectData) map[string]ProviderState {
	providers := map[string]ProviderState{}
	names := []string{"claude", "codex", "opencode", "agy"}

	for _, name := range names {
		state := ProviderState{
			Installed: false,
			LoggedIn:  "unknown",
			Version:   "unknown",
		}
		if _, err := d.LookPath(name); err == nil {
			state.Installed = true
			if out, err := d.ExecCommand(ctx, name, "--version"); err == nil {
				if version := strings.TrimSpace(string(out)); version != "" {
					state.Version = version
				}
			} else if out, err := d.ExecCommand(ctx, name, "version"); err == nil {
				if version := strings.TrimSpace(string(out)); version != "" {
					state.Version = version
				}
			}
		}

		if name == "codex" {
			if sandboxData.Codex != "" {
				state.Sandbox = sandboxData.Codex
			} else {
				state.Sandbox = "unknown"
			}
		} else {
			if sandboxData.Enabled {
				if sandboxData.BwrapUsable && !sandboxData.PlanUnavailable {
					state.Sandbox = "wrapped"
				} else {
					state.Sandbox = "unavailable"
				}
			} else {
				state.Sandbox = "not protected"
			}
		}

		providers[name] = state
	}
	return providers
}

func (d *Detectors) DetectHosting(ctx context.Context, launchHostShim bool) HostingState {
	state := HostingState{
		LaunchHostShim:     launchHostShim,
		SystemdUserSession: "unknown",
		LingerEnabled:      "unknown",
	}
	// A runtime directory alone is not proof of a reachable user manager.
	out, _ := d.ExecCommand(ctx, "systemctl", "--user", "is-system-running")
	switch strings.TrimSpace(string(out)) {
	case "running", "degraded":
		state.SystemdUserSession = "true"
	case "offline":
		state.SystemdUserSession = "false"
	}
	out, err := d.ExecCommand(ctx, "loginctl", "show-user", strconv.Itoa(os.Getuid()), "--property=Linger")
	if err == nil {
		switch strings.TrimSpace(string(out)) {
		case "Linger=yes":
			state.LingerEnabled = "true"
		case "Linger=no":
			state.LingerEnabled = "false"
		}
	}
	return state
}

func (d *Detectors) DetectFilesystem(ctx context.Context, workspaceRoot string) FilesystemState {
	state := FilesystemState{ReflinkSupported: "unknown"}
	f1, err := os.CreateTemp(workspaceRoot, ".reflink_test_1_*")
	if err != nil {
		return state
	}
	f1Name := f1.Name()
	f1.Close()
	defer func() { _ = os.Remove(f1Name) }()

	f2, err := os.CreateTemp(workspaceRoot, ".reflink_test_2_*")
	if err != nil {
		return state
	}
	f2Name := f2.Name()
	f2.Close()
	defer func() { _ = os.Remove(f2Name) }()

	if err := os.WriteFile(f1Name, []byte("test"), 0600); err == nil {
		out, err := d.ExecCommand(ctx, "cp", "--reflink=always", f1Name, f2Name)
		if err != nil {
			// Tool absence, cancellation and permissions do not establish a
			// filesystem limitation. Only an explicit unsupported clone does.
			var exitErr *exec.ExitError
			if errors.Is(err, unix.EOPNOTSUPP) || (errors.As(err, &exitErr) && strings.Contains(strings.ToLower(string(append(out, exitErr.Stderr...))), "operation not supported")) {
				state.ReflinkSupported = "false"
			}
		} else {
			state.ReflinkSupported = "true"
		}
	}
	return state
}

func (d *Detectors) MeasureResources(stateRoot, workRoot string) ResourceState {
	st := ResourceState{
		CPUCount: runtime.NumCPU(),
		CPULoad:  "unknown",
		Status:   "partial",
	}

	readFile := d.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	measured := 0
	// Unix specific loadavg
	if b, err := readFile("/proc/loadavg"); err == nil {
		if fields := strings.Fields(string(b)); len(fields) > 0 {
			st.CPULoad = fields[0]
			measured++
		}
	}

	if b, err := readFile("/proc/meminfo"); err == nil {
		lines := strings.Split(string(b), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "MemAvailable:") {
				var kb uint64
				if n, err := fmt.Sscanf(line, "MemAvailable: %d kB", &kb); n == 1 && err == nil {
					available := kb * 1024
					st.MemoryAvailable = &available
					measured++
				}
				break
			}
		}
	}

	var stat unix.Statfs_t
	if err := d.Statfs(stateRoot, &stat); err == nil {
		free := stat.Bavail * uint64(stat.Bsize) //nolint:gosec
		st.StateDiskFree = &free
		measured++
	}
	if err := d.Statfs(workRoot, &stat); err == nil {
		free := stat.Bavail * uint64(stat.Bsize) //nolint:gosec
		st.WorkDiskFree = &free
		measured++
	}

	if measured == 4 {
		st.Status = "ok"
	}
	return st
}
