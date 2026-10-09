//go:build linux

package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/harness/runner"
	"github.com/hollis-labs/substrate/harness/sandbox"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/shimhost"
	"golang.org/x/sys/unix"
)

func TestShimOtherSandboxedLaunchDeniesPrivateRoot(t *testing.T) {
	t.Setenv(EnvLaunchHost, "shim")
	svc, _, _ := tetherLayout(t)
	privateRoot := filepath.Join(filepath.Dir(svc.Catalog.Global.Catalog.Defaults.StateDB), "shims")
	// Resolve the catalog's symlink before creating private state.
	privateRoot = realPathOrClean(privateRoot)
	descriptor := filepath.Join(privateRoot, "session", "launch.json")
	if err := shimhost.WritePrivateJSON(descriptor, map[string]string{"secret": "fake-capability"}); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	opts := agentsessions.StartOptions{Workdir: work, WorkspaceDir: work}
	if err := svc.applyControlPlaneProtection(cliPlan, "cli", &opts); err != nil {
		t.Fatal(err)
	}
	if opts.SandboxPolicy == nil || opts.SandboxPolicy.AccessFor(descriptor) != sandbox.AccessDenied {
		t.Fatal("another sandboxed launch can read private shim state")
	}
	t.Run("real_backend", func(t *testing.T) {
		requireShimProtectingBackend(t, privateRoot)
		cmd := exec.Command("/bin/sh", "-c", `if cat "$1" >/dev/null 2>&1; then exit 9; fi`, "probe", descriptor)
		cmd.Dir = work
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + work, "TMPDIR=" + work}
		_, cleanup, err := sandbox.ApplyResolved(cmd, *opts.SandboxPolicy)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if err := cmd.Run(); err != nil {
			t.Fatalf("other sandbox read the descriptor: %v", err)
		}
	})
}

func TestShimImplicitWorkspacePolicyIsCompleteBeforePlacement(t *testing.T) {
	// Allocate outside the private parent before the fixture changes TMPDIR.
	work := t.TempDir()
	f := shimFixture(t)
	f.req.Options.Workdir = work
	f.req.Options.Profile = sandbox.Profile{ID: "legacy-workspace", Net: true, Subprocess: true}
	prepare := f.svc.shimHosting.prepare
	called := false
	f.svc.shimHosting.prepare = func(spec shim.Launch, policy *sandbox.ResolvedAccessPolicy, limits runner.ResourceLimits) (shim.Launch, func(), error) {
		called = true
		if policy.AccessFor(work) != sandbox.AccessReadWrite {
			t.Fatal("implicit workspace disappeared during policy translation")
		}
		return prepare(spec, policy, limits)
	}
	if _, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req); err != nil {
		t.Fatal(err)
	}
	if !called {
		_, err := shimSandboxPolicy(f.req.Options, filepath.Dir(f.svc.shimHosting.provider.SessionDir(f.req.ID)))
		t.Fatalf("workspace translation fell back instead of preparing a complete policy: %v", err)
	}
}

func TestShimMissingDenyPathRefusesBeforePlacementWithReason(t *testing.T) {
	// Allocate outside the private parent before the fixture changes TMPDIR.
	work := t.TempDir()
	f := shimFixture(t)
	f.req.Options.Workdir = work
	f.req.Options.Profile = sandbox.Profile{ID: "workspace", Net: true, Subprocess: true, FS: sandbox.FSSpec{Deny: []string{filepath.Join(f.root, "absent-credentials")}}}
	f.svc.shimHosting.place = func(context.Context, string, shim.Launch) (shimhost.Receipt, error) {
		t.Fatal("underspecified policy reached placement")
		return shimhost.Receipt{}, nil
	}
	req, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
	if err != nil || req.Runtime != f.req.Runtime {
		t.Fatalf("policy refusal did not use direct path: %v", err)
	}
	found := false
	for _, status := range shimStatusEvents(t, f) {
		if status.State == "direct_fallback" && status.Reason == "policy_path_missing" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing deny path lost its distinct diagnostic")
	}
}

func TestShimGoneRemovesSecretDescriptor(t *testing.T) {
	f := shimFixture(t)
	receipt := f.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.svc.DrainSessions(ctx); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(receipt.HostPID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(receipt.HostPID) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+2:])
	parent, err := strconv.Atoi(fields[1])
	if err != nil || parent != os.Getpid() {
		t.Fatalf("host is not test-owned: %v", err)
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start != receipt.HostStartTime {
		t.Fatalf("host identity changed: %v", err)
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil {
		t.Fatal(err)
	}
	shimAwait(t, "positive host absence", func() bool {
		result, err := f.svc.shimHosting.provider.Inspect(context.Background(), receipt)
		return err == nil && result.Gone
	})
	f.svc.ReconcileStaleState()
	row, err := f.svc.Store.GetSession(f.req.ID)
	if err != nil || row.State != "orphaned" {
		t.Fatalf("positive absence: %+v %v", row, err)
	}
	if _, err := os.Stat(receipt.DescriptorPath); !os.IsNotExist(err) {
		t.Fatalf("orphan retained provider secrets: %v", err)
	}
}

func TestShimStateParentProtectionRealProbe(t *testing.T) {
	for _, protected := range []bool{false, true} {
		t.Run(strconv.FormatBool(protected), func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "state")
			private := filepath.Join(parent, "shims")
			if err := os.MkdirAll(private, 0700); err != nil {
				t.Fatal(err)
			}
			work := t.TempDir()
			opts := agentsessions.StartOptions{Workdir: work, Profile: sandbox.Profile{ID: "probe", HostFilesystem: true, Net: true, Subprocess: true}}
			policy, err := shimSandboxPolicy(opts, private)
			if err != nil {
				t.Fatal(err)
			}
			if !protected {
				policy.FS.Protect = nil
			}
			if !policy.DenyUserServiceManager || policy.AccessFor(private) != sandbox.AccessDenied {
				t.Fatal("policy lost private-state or service-manager denial")
			}
			want := sandbox.AccessReadWrite
			if protected {
				want = sandbox.AccessReadOnly
			}
			if policy.AccessFor(parent) != want {
				t.Fatalf("parent access = %s, want %s", policy.AccessFor(parent), want)
			}
			t.Run("real_backend", func(t *testing.T) {
				requireShimProtectingBackend(t, parent)
				cmd := exec.Command("/bin/sh", "-c", `if touch "$1/probe" 2>/dev/null; then echo write; fi
	if mv "$1" "$2" 2>/dev/null; then echo rename; fi`, "probe", parent, filepath.Join(root, "renamed"))
				cmd.Dir = work
				cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + work, "TMPDIR=" + work}
				_, cleanup, err := sandbox.ApplyResolved(cmd, policy)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				output, err := cmd.Output()
				if err != nil {
					t.Fatal(err)
				}
				wrote := strings.Contains(string(output), "write")
				renamed := strings.Contains(string(output), "rename")
				t.Logf("protected=%t write=%t rename=%t", protected, wrote, renamed)
				if protected && (wrote || renamed) {
					t.Fatal("protected state parent was mutable")
				}
				if !protected && (!wrote || !renamed) {
					t.Fatal("unprotected control did not demonstrate parent write and rename access")
				}
			})
		})
	}
}

func requireShimProtectingBackend(t *testing.T, dir string) {
	t.Helper()
	// Probe the actual namespace capability, bypassing fixture protection stubs.
	if err := ProbeBwrap(dir); err != nil {
		t.Skipf("real protecting backend unavailable on this host: %v", err)
	}
}
