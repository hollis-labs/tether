package report

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

type Detectors struct {
	ExecCommand func(ctx context.Context, name string, arg ...string) ([]byte, error)
	LookPath    func(file string) (string, error)
	Stat        func(name string) (os.FileInfo, error)
	Statfs      func(path string, buf *unix.Statfs_t) error
}

func DefaultDetectors() *Detectors {
	return &Detectors{
		ExecCommand: func(ctx context.Context, name string, arg ...string) ([]byte, error) {
			//nolint:gosec // intentional
			return exec.CommandContext(ctx, name, arg...).Output()
		},
		LookPath: exec.LookPath,
		Stat:     os.Stat,
		Statfs:   unix.Statfs,
	}
}

type SandboxProtectData struct {
	Enabled     bool
	BwrapUsable bool
	Codex       string
}

func (d *Detectors) DetectProviders(ctx context.Context, sandboxData SandboxProtectData) map[string]ProviderState {
	providers := map[string]ProviderState{}
	names := []string{"claude", "codex", "opencode", "agy"}

	for _, name := range names {
		state := ProviderState{
			Installed: false,
			LoggedIn:  "unknown",
		}
		if _, err := d.LookPath(name); err == nil {
			state.Installed = true
			if out, err := d.ExecCommand(ctx, name, "--version"); err == nil {
				state.Version = strings.TrimSpace(string(out))
			} else if out, err := d.ExecCommand(ctx, name, "version"); err == nil {
				state.Version = strings.TrimSpace(string(out))
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
				if sandboxData.BwrapUsable {
					state.Sandbox = "wrapped"
				} else {
					state.Sandbox = "protected"
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
		LaunchHostShim: launchHostShim,
	}
	if os.Getenv("XDG_RUNTIME_DIR") != "" {
		state.SystemdUserSession = true
	}
	out, err := d.ExecCommand(ctx, "loginctl", "show-user", os.Getenv("USER"), "--property=Linger")
	if err == nil {
		if strings.Contains(string(out), "Linger=yes") {
			state.LingerEnabled = true
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
		_, err := d.ExecCommand(ctx, "cp", "--reflink=always", f1Name, f2Name)
		if err != nil {
			state.ReflinkSupported = "false"
		} else {
			state.ReflinkSupported = "true"
		}
	}
	return state
}

func (d *Detectors) MeasureResources(stateRoot, workRoot string) ResourceState {
	st := ResourceState{
		CPUCount: runtime.NumCPU(),
	}

	// Unix specific loadavg
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		st.CPULoad = strings.SplitN(string(b), " ", 2)[0]
	}

	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		lines := strings.Split(string(b), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "MemAvailable:") {
				var kb uint64
				_, _ = fmt.Sscanf(line, "MemAvailable: %d kB", &kb)
				st.MemoryAvailable = kb * 1024
				break
			}
		}
	}

	var stat unix.Statfs_t
	if err := d.Statfs(stateRoot, &stat); err == nil {
		st.StateDiskFree = stat.Bavail * uint64(stat.Bsize) //nolint:gosec
	}
	if err := d.Statfs(workRoot, &stat); err == nil {
		st.WorkDiskFree = stat.Bavail * uint64(stat.Bsize) //nolint:gosec
	}

	return st
}
