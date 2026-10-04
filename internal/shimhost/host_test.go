//go:build linux

package shimhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/go-runner/runner"
	"github.com/hollis-labs/go-sandbox/sandbox"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/testutil"
	"golang.org/x/sys/unix"
)

func TestHostProcess(t *testing.T) {
	if os.Getenv("TETHER_TEST_SHIM") != "" {
		parent := os.Getppid()
		if parent <= 1 || unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0, 0, 0) != nil || os.Getppid() != parent {
			os.Exit(96)
		}
	}
	switch os.Getenv("TETHER_TEST_SHIM") {
	case "host":
		path := ""
		for i, a := range os.Args {
			if a == "--launch" && i+1 < len(os.Args) {
				path = os.Args[i+1]
			}
		}
		spec, e := shim.ReadLaunch(path)
		if e != nil {
			fmt.Fprintln(os.Stderr, "descriptor refused")
			os.Exit(2)
		}
		h, e := shim.Start(spec)
		if e != nil {
			fmt.Fprintln(os.Stderr, "host refused")
			os.Exit(3)
		}
		if os.Getenv("TETHER_TEST_IGNORE_TERM") != "" {
			signal.Ignore(syscall.SIGTERM)
			select {}
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		_ = h.Close()
		os.Exit(0)
	case "child-exit":
		os.Exit(7)
	case "child":
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
}
func hostSpec(t *testing.T) (Config, shim.Launch) {
	t.Helper()
	root := testutil.ShortDir(t)
	t.Cleanup(func() {
		if e := os.RemoveAll(root); e != nil {
			t.Error(e)
		}
	})
	if e := os.WriteFile(filepath.Join(root, "pin"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	spec := shim.Launch{Session: "urn:session:host", Instance: "urn:instance:host", Generation: 1, Actor: mesh.Actor{URN: "msg://service/shim/test", Kind: mesh.ActorService}, Subject: "urn:session:host", Argv: []string{exe, "-test.run=^TestHostProcess$"}, Env: []string{"TETHER_TEST_SHIM=child", "HOME=" + root, "TMPDIR=" + root}, Cwd: root, PinPath: filepath.Join(root, "pin"), PinKey: "test", BootGeneration: "test", Reservation: "test", StopGrace: 50 * time.Millisecond, Heartbeat: time.Second}
	cfg := Config{StateDir: filepath.Join(root, "s"), ShimCommand: []string{exe, "-test.run=^TestHostProcess$", "--"}, HostEnv: []string{"TETHER_TEST_SHIM=host", "HOME=" + root, "TMPDIR=" + root}}
	return cfg, spec
}
func TestDetachedPlaceInspectReattachStop(t *testing.T) {
	cfg, spec := hostSpec(t)
	p, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, e := p.Place(ctx, "operation-one", spec)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if e := p.Stop(ctx, r); e != nil {
			t.Error(e)
		}
	})
	t.Logf("host=%d shim=%d provider=%d", r.HostPID, r.ShimPID, r.ProviderPID)
	if r.HostPID == 0 || r.ShimPID != r.HostPID || r.ProviderPID == 0 || r.HostPID == r.ProviderPID {
		t.Fatal("host/provider pids not tracked")
	}
	again, e := p.Place(ctx, "operation-one", spec)
	if e != nil {
		t.Fatal(e)
	}
	if again.HostPID != r.HostPID || again.ProviderPID != r.ProviderPID || again.Journal != r.Journal {
		t.Fatal("idempotent placement spawned again")
	}
	inspection, e := p.Reattach(ctx, r)
	if e != nil || !inspection.Running {
		t.Fatalf("reattach: %+v %v", inspection, e)
	}
	desc, e := Descriptor(r)
	if e != nil {
		t.Fatal(e)
	}
	if desc.JournalBytes != DefaultJournalBytes {
		t.Fatal("default journal cap not applied")
	}
	for _, path := range []string{cfg.StateDir, filepath.Dir(r.DescriptorPath), desc.ControlDir, desc.JournalDir} {
		info, e := os.Lstat(path)
		if e != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("private directory %s: %v", path, e)
		}
	}
	for _, path := range []string{r.DescriptorPath, metadataPath(r), filepath.Join(filepath.Dir(r.DescriptorPath), "host.log")} {
		info, e := os.Lstat(path)
		if e != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private file %s: %v", path, e)
		}
	}
	argv, e := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", r.HostPID))
	if e != nil {
		t.Fatal(e)
	}
	log, e := os.ReadFile(filepath.Join(filepath.Dir(r.DescriptorPath), "host.log"))
	if e != nil {
		t.Fatal(e)
	}
	receipt, _ := json.Marshal(r)
	if strings.Contains(string(argv)+string(log)+string(receipt), desc.Secret) {
		t.Fatal("capability escaped descriptor")
	}
	spec.Argv = append(spec.Argv, "changed")
	if _, e = p.Place(ctx, "operation-one", spec); e == nil {
		t.Fatal("changed placement accepted")
	}
	if e = p.Stop(ctx, r); e != nil {
		t.Fatal(e)
	}
	if !errors.Is(syscall.Kill(r.ProviderPID, 0), syscall.ESRCH) || !errors.Is(syscall.Kill(r.HostPID, 0), syscall.ESRCH) {
		t.Fatal("stop leaked host or child")
	}
	inspection, e = p.Inspect(ctx, r)
	if e != nil || !inspection.Gone {
		t.Fatalf("gone inspect: %+v %v", inspection, e)
	}
}
func TestUncertainSubmitIsNeverRepeated(t *testing.T) {
	cfg, spec := hostSpec(t)
	cfg.Backend = SystemdUser
	if _, e := New(cfg); e == nil {
		t.Fatal("systemd enabled without switch")
	}
	cfg.AllowSystemd = true
	submits := 0
	unit := ""
	cfg.Command = func(_ context.Context, argv []string) ([]byte, error) {
		if argv[0] == "systemd-run" {
			submits++
			for _, a := range argv {
				if strings.HasPrefix(a, "--unit=") {
					unit = strings.TrimPrefix(a, "--unit=")
				}
			}
			if !strings.Contains(strings.Join(argv, " "), "--property=KillMode=control-group") {
				t.Fatal("unit boundary missing")
			}
			return nil, errors.New("submit response lost")
		}
		return []byte("not-found\n"), nil
	}
	p, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	_, e = p.Place(context.Background(), "operation-uncertain", spec)
	var unknown *Failure
	if !errors.As(e, &unknown) || unknown.Code != "outcome_unknown" {
		t.Fatalf("submit: %v", e)
	}
	_, e = p.Place(context.Background(), "operation-uncertain", spec)
	if !errors.As(e, &unknown) || unknown.Code != "outcome_unknown" {
		t.Fatalf("retry: %v", e)
	}
	if submits != 1 || !strings.HasPrefix(unit, "tether-shim-"+hash(spec.Instance)+"-") {
		t.Fatal("uncertain submission repeated or foreign unit")
	}
}
func TestDescriptorRefusesUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if e := os.WriteFile(target, []byte(`{}`), 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := ReadDescriptor(target); e == nil {
		t.Fatal("loose descriptor accepted")
	}
	if e := os.Chmod(target, 0600); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(dir, "link")
	if e := os.Symlink(target, link); e != nil {
		t.Fatal(e)
	}
	if _, e := ReadDescriptor(link); e == nil {
		t.Fatal("descriptor symlink accepted")
	}
	if e := WritePrivateJSON(link, map[string]string{"secret": "test"}); e == nil {
		t.Fatal("write followed descriptor symlink")
	}
}
func TestPrepareProviderWrapsRealCommand(t *testing.T) {
	_, spec := hostSpec(t)
	providerExe := spec.Argv[0]
	prepared, cleanup, e := PrepareProvider(spec, nil, runner.ResourceLimits{MaxOpenFiles: 256})
	if e != nil {
		t.Fatal(e)
	}
	defer cleanup()
	if prepared.Argv[0] == providerExe || !strings.Contains(strings.Join(prepared.Argv, " "), providerExe) || !strings.Contains(strings.Join(prepared.Argv, " "), "ulimit -n 256") {
		t.Fatal("limits not applied to real provider argv")
	}
}

func TestRequiredProviderSandboxRefusesUnsupportedBackend(t *testing.T) {
	_, spec := hostSpec(t)
	policy := &sandbox.ResolvedAccessPolicy{ID: "required-test", Mode: sandbox.ConfinementRequired, Backend: sandbox.BackendNone}
	_, cleanup, err := PrepareProvider(spec, policy, runner.ResourceLimits{})
	defer cleanup()
	if err == nil {
		t.Fatal("required provider isolation silently weakened")
	}
}
