package report

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDetectProviders(t *testing.T) {
	d := &Detectors{
		LookPath: func(file string) (string, error) {
			if file == "claude" || file == "agy" {
				return "/bin/" + file, nil
			}
			return "", errors.New("not found")
		},
		ExecCommand: func(ctx context.Context, name string, arg ...string) ([]byte, error) {
			if name == "claude" {
				return []byte("1.2.3\n"), nil
			}
			if name == "agy" {
				return []byte("2.0.0"), nil
			}
			return nil, errors.New("error")
		},
	}

	sd := SandboxProtectData{
		Enabled:     true,
		BwrapUsable: true,
		Codex:       "guarded",
	}

	ctx := context.Background()
	provs := d.DetectProviders(ctx, sd)

	if !provs["claude"].Installed || provs["claude"].Version != "1.2.3" || provs["claude"].Sandbox != "wrapped" {
		t.Errorf("claude unexpected: %+v", provs["claude"])
	}
	if provs["codex"].Installed || provs["codex"].Sandbox != "guarded" {
		t.Errorf("codex unexpected: %+v", provs["codex"])
	}
	if !provs["agy"].Installed || provs["agy"].Version != "2.0.0" {
		t.Errorf("agy unexpected: %+v", provs["agy"])
	}
}

func TestDetectHosting(t *testing.T) {
	for _, tc := range []struct{ name, systemd, linger, wantSystemd, wantLinger string }{
		{"available", "running", "Linger=yes", "true", "true"},
		{"degraded", "degraded", "Linger=no", "true", "false"},
		{"offline", "offline", "Linger=no", "false", "false"},
		{"undetectable", "", "", "unknown", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", "/synthetic/runtime")
			d := &Detectors{ExecCommand: func(ctx context.Context, name string, arg ...string) ([]byte, error) {
				if name == "systemctl" {
					return []byte(tc.systemd), errors.New("synthetic nonzero status")
				}
				if !reflect.DeepEqual(arg, []string{"show-user", strconv.Itoa(os.Getuid()), "--property=Linger"}) {
					t.Fatalf("linger probe did not select the actual user: %v", arg)
				}
				if tc.linger == "" {
					return nil, errors.New("unavailable")
				}
				return []byte(tc.linger), nil
			}}
			host := d.DetectHosting(context.Background(), true)
			if host.SystemdUserSession != tc.wantSystemd || host.LingerEnabled != tc.wantLinger || !host.LaunchHostShim {
				t.Fatalf("hosting = %+v", host)
			}
		})
	}
}

func TestDetectFilesystem(t *testing.T) {
	d := &Detectors{
		ExecCommand: func(ctx context.Context, name string, arg ...string) ([]byte, error) {
			return nil, nil // successful cp
		},
	}
	ctx := context.Background()
	fs := d.DetectFilesystem(ctx, t.TempDir())

	if fs.ReflinkSupported != "true" {
		t.Errorf("fs unexpected: %+v", fs)
	}
}

func TestCapabilityGroups(t *testing.T) {
	caps := CapabilityGroups()
	want := map[string]map[string]any{"capability_report": {
		"version": 1, "providers": true, "sandbox": true, "hosting": true,
		"filesystem": true, "resources": true, "role_profile": true,
	}}
	if !reflect.DeepEqual(caps, want) {
		t.Fatalf("public report contract = %v", caps)
	}
	caps["capability_report"]["providers"] = false
	if CapabilityGroups()["capability_report"]["providers"] != true {
		t.Fatal("capability maps shared across compositions")
	}
}

func TestDetectFilesystem_SafetyAndErrors(t *testing.T) {
	workspaceRoot := t.TempDir()
	sentinel := workspaceRoot + "/.reflink_test_1"
	_ = os.WriteFile(sentinel, []byte("sentinel"), 0644)

	probeCalled := false
	d := &Detectors{
		ExecCommand: func(ctx context.Context, name string, arg ...string) ([]byte, error) {
			probeCalled = true
			return nil, errors.New("fail")
		},
	}

	ctx := context.Background()
	fs := d.DetectFilesystem(ctx, workspaceRoot)

	if fs.ReflinkSupported != "unknown" {
		t.Errorf("expected unknown for undetectable failure, got %s", fs.ReflinkSupported)
	}
	if !probeCalled {
		t.Errorf("expected probe to be called")
	}

	b, err := os.ReadFile(sentinel)
	if err != nil || string(b) != "sentinel" {
		t.Errorf("sentinel file was damaged")
	}
}

func TestDetectFilesystem_FailedScratch(t *testing.T) {
	// An unavailable selected workspace cannot be probed on another volume.
	workspaceRoot := t.TempDir() + "/unavailable"

	probeCalled := false
	d := &Detectors{
		ExecCommand: func(ctx context.Context, name string, arg ...string) ([]byte, error) {
			probeCalled = true
			return nil, nil
		},
	}

	ctx := context.Background()
	fs := d.DetectFilesystem(ctx, workspaceRoot)

	if fs.ReflinkSupported != "unknown" {
		t.Errorf("expected unknown, got %s", fs.ReflinkSupported)
	}
	if probeCalled {
		t.Errorf("expected probe NOT to be called when scratch fails")
	}
}

func TestDetectFilesystem_UnsupportedClone(t *testing.T) {
	d := &Detectors{ExecCommand: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("cp: failed to clone: Operation not supported"), &exec.ExitError{}
	}}
	if got := d.DetectFilesystem(context.Background(), t.TempDir()).ReflinkSupported; got != "false" {
		t.Fatalf("explicit unsupported clone = %s", got)
	}
}

func TestDetectProviders_UnavailableSandbox(t *testing.T) {
	d := &Detectors{LookPath: func(string) (string, error) { return "", exec.ErrNotFound }}
	for _, sd := range []SandboxProtectData{
		{Enabled: true}, {Enabled: true, BwrapUsable: true, PlanUnavailable: true},
	} {
		got := d.DetectProviders(context.Background(), sd)
		if got["claude"].Sandbox != "unavailable" || got["codex"].Sandbox != "unknown" || got["codex"].Version != "unknown" {
			t.Fatalf("unavailable protection became success: %+v", got)
		}
	}
}

func TestMeasureResources_UnknownAndZero(t *testing.T) {
	selected := []string{}
	d := &Detectors{
		ReadFile: func(string) ([]byte, error) { return nil, errors.New("unavailable") },
		Statfs: func(path string, buf *unix.Statfs_t) error {
			selected = append(selected, path)
			return errors.New("unavailable")
		},
	}
	got := d.MeasureResources("selected-state", "selected-work")
	if got.Status != "partial" || got.CPULoad != "unknown" || got.MemoryAvailable != nil || got.StateDiskFree != nil || got.WorkDiskFree != nil {
		t.Fatalf("failed measurements became zero: %+v", got)
	}
	if !reflect.DeepEqual(selected, []string{"selected-state", "selected-work"}) {
		t.Fatal(selected)
	}
	d.ReadFile = func(path string) ([]byte, error) {
		if path == "/proc/loadavg" {
			return []byte("0.00 0.00 0.00 1/1 1"), nil
		}
		return []byte("MemAvailable: 0 kB\n"), nil
	}
	d.Statfs = func(string, *unix.Statfs_t) error { return nil }
	got = d.MeasureResources("selected-state", "selected-work")
	if got.Status != "ok" || got.MemoryAvailable == nil || *got.MemoryAvailable != 0 || got.StateDiskFree == nil || *got.StateDiskFree != 0 || got.WorkDiskFree == nil {
		t.Fatalf("known zero became unknown: %+v", got)
	}
}

func TestDefaultDetectors_BoundedSyntheticProbe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TETHER_REPORT_SYNTHETIC_PROBE", "output")
	out, err := DefaultDetectors().ExecCommand(context.Background(), executable, "-test.run=^TestReportSyntheticProbeHelper$")
	if err != nil || len(out) != 32*1024 {
		t.Fatalf("probe output bytes=%d err=%v", len(out), err)
	}
	t.Setenv("TETHER_REPORT_SYNTHETIC_PROBE", "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := DefaultDetectors().ExecCommand(ctx, executable, "-test.run=^TestReportSyntheticProbeHelper$"); err == nil {
		t.Fatal("canceled synthetic probe succeeded")
	}
}

func TestReportSyntheticProbeHelper(t *testing.T) {
	switch os.Getenv("TETHER_REPORT_SYNTHETIC_PROBE") {
	case "output":
		fmt.Print(strings.Repeat("x", 64*1024))
		os.Exit(0)
	case "wait":
		time.Sleep(time.Second)
		os.Exit(0)
	}
}

func TestDetectProviders_EmptyVersionIsUnknown(t *testing.T) {
	d := &Detectors{
		LookPath:    func(string) (string, error) { return "/synthetic/tool", nil },
		ExecCommand: func(context.Context, string, ...string) ([]byte, error) { return []byte("\n"), nil },
	}
	for name, state := range d.DetectProviders(context.Background(), SandboxProtectData{}) {
		if !state.Installed || state.Version != "unknown" || state.LoggedIn != "unknown" {
			t.Fatalf("%s = %+v", name, state)
		}
	}
}

func TestDetectProviders_NoPositionalPromptFallback(t *testing.T) {
	calls := 0
	d := &Detectors{
		LookPath: func(string) (string, error) { return "/synthetic/tool", nil },
		ExecCommand: func(ctx context.Context, name string, arg ...string) ([]byte, error) {
			calls++
			if !reflect.DeepEqual(arg, []string{"--version"}) {
				t.Fatalf("potential model prompt invocation: %s %v", name, arg)
			}
			return nil, errors.New("unsupported version flag")
		},
	}
	got := d.DetectProviders(context.Background(), SandboxProtectData{})
	for name, state := range got {
		if state.Version != "unknown" {
			t.Fatalf("%s version = %q", name, state.Version)
		}
	}
	if calls != len(got) {
		t.Fatal("unsupported version command was retried", calls)
	}
}
