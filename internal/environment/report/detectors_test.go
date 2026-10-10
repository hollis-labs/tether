package report

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
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
	d := &Detectors{
		ExecCommand: func(ctx context.Context, name string, arg ...string) ([]byte, error) {
			return []byte("Linger=yes\n"), nil
		},
	}
	os.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	defer os.Unsetenv("XDG_RUNTIME_DIR")

	ctx := context.Background()
	host := d.DetectHosting(ctx, true)

	if !host.SystemdUserSession || !host.LingerEnabled || !host.LaunchHostShim {
		t.Errorf("hosting unexpected: %+v", host)
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
	r := Report{
		Providers: map[string]ProviderState{
			"claude": {Installed: true},
			"codex":  {Installed: false},
		},
		Hosting: HostingState{
			LaunchHostShim: true,
		},
		Filesystem: FilesystemState{
			ReflinkSupported: "true",
		},
		RoleProfile: RoleProfile{
			Enabled: []string{"teams"},
		},
	}

	caps := r.CapabilityGroups()

	if !reflect.DeepEqual(caps["providers"], map[string]any{"claude": true}) {
		t.Errorf("providers caps wrong: %v", caps["providers"])
	}
	if !reflect.DeepEqual(caps["hosting"], map[string]any{"shim": true}) {
		t.Errorf("hosting caps wrong: %v", caps["hosting"])
	}
	if !reflect.DeepEqual(caps["filesystem"], map[string]any{"reflink": true}) {
		t.Errorf("fs caps wrong: %v", caps["filesystem"])
	}
	if !reflect.DeepEqual(caps["modules"], map[string]any{"teams": true}) {
		t.Errorf("modules caps wrong: %v", caps["modules"])
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

	if fs.ReflinkSupported != "false" {
		t.Errorf("expected false, got %s", fs.ReflinkSupported)
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
	// Provide a readonly workspaceRoot so CreateTemp fails
	workspaceRoot := t.TempDir()
	os.Chmod(workspaceRoot, 0400)

	probeCalled := false
	d := &Detectors{
		ExecCommand: func(ctx context.Context, name string, arg ...string) ([]byte, error) {
			probeCalled = true
			return nil, nil
		},
	}

	ctx := context.Background()
	fs := d.DetectFilesystem(ctx, workspaceRoot)
	os.Chmod(workspaceRoot, 0700)

	if fs.ReflinkSupported != "unknown" {
		t.Errorf("expected unknown, got %s", fs.ReflinkSupported)
	}
	if probeCalled {
		t.Errorf("expected probe NOT to be called when scratch fails")
	}
}
