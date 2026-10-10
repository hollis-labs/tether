package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

type scratchAdmissionRig struct {
	service  *Service
	row      *store.SessionRow
	decision store.WorkspaceLaunchDecision
	plan     *launch.Plan
	control  string
	unlock   func()
}

type scratchFixtureRuntime struct {
	start   func(agentsessions.StartOptions) (agentsessions.Session, error)
	kind    string
	prepare func() error
}

func (scratchFixtureRuntime) ID() string { return "scratch-fixture" }
func (r scratchFixtureRuntime) Kind() string {
	if r.kind != "" {
		return r.kind
	}
	return "cli"
}
func (r scratchFixtureRuntime) Prepare(context.Context) error {
	if r.prepare != nil {
		return r.prepare()
	}
	return nil
}
func (scratchFixtureRuntime) Caps() agentsessions.Capabilities {
	return agentsessions.Capabilities{JsonRpcStdio: true}
}
func (r scratchFixtureRuntime) Start(_ context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	return r.start(opts)
}

type scratchFixtureSession struct {
	*strictNativeCoauthorSession
	waits atomic.Int32
}

func (s *scratchFixtureSession) Wait() (int, error) {
	s.waits.Add(1)
	return s.strictNativeCoauthorSession.Wait()
}

type scratchFixtureEvents func(agentsessions.LifecycleEvent)

func (f scratchFixtureEvents) Emit(_ context.Context, event agentsessions.LifecycleEvent) { f(event) }

func scratchIsClosed(custody *launchScratchCustody) bool {
	custody.mu.Lock()
	defer custody.mu.Unlock()
	return custody.closed
}

func TestLaunchScratchRuntimePreservesSessionAndAuthoritativeWait(t *testing.T) {
	rig := newScratchAdmissionRig(t)
	custody := rig.admit(t, uuid.NewString())
	if err := rig.service.escrowLaunchScratch(custody); err != nil {
		t.Fatal(err)
	}
	original := &scratchFixtureSession{strictNativeCoauthorSession: &strictNativeCoauthorSession{recoveryFakeSession: &recoveryFakeSession{done: make(chan struct{})}}}
	runtime := &launchScratchRuntime{custody: custody, Runtime: scratchFixtureRuntime{start: func(agentsessions.StartOptions) (agentsessions.Session, error) {
		// Provider callbacks can use Store's sole connection: Start owns no
		// transaction or c.mu lock across this invocation.
		if _, err := rig.service.Store.WorkspaceLaunchDecision(context.Background(), rig.row.ID); err != nil {
			t.Fatal(err)
		}
		return original, nil
	}}}
	rig.service.Manager = agentsessions.NewManager(stateSinkAdapter{db: rig.service.Store})
	if err := rig.service.Manager.Start(context.Background(), agentsessions.StartRequest{ID: rig.row.ID, Runtime: runtime}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.service.Manager.JsonRpcCall(context.Background(), rig.row.ID, "initialize", nil); err != nil {
		t.Fatalf("optional RPC facet erased: %v", err)
	}
	original.mu.Lock()
	calledOriginal := len(original.methods) == 1 && original.methods[0] == "initialize"
	original.mu.Unlock()
	if !calledOriginal {
		t.Fatal("manager RPC call did not reach original session")
	}
	if err := original.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	rig.service.releaseScratchAfterCompletion(context.Background(), custody)
	if !scratchIsClosed(custody) || rig.service.holdsLaunchScratch(rig.row.ID) || original.waits.Load() != 1 {
		t.Fatal("completion release lost ownership or called Session.Wait twice")
	}
	if _, err := os.Stat(custody.allocation.Root); err != nil {
		t.Fatal("completion deleted retained allocation")
	}
}

func TestLaunchScratchRuntimeRefusalAndUnknownEntry(t *testing.T) {
	for _, scenario := range []string{"unknown-start", "changed-row", "foreign-native-stamp", "root-substitution", "holder-change"} {
		t.Run(scenario, func(t *testing.T) {
			rig := newScratchAdmissionRig(t)
			custody := rig.admit(t, uuid.NewString())
			if err := rig.service.escrowLaunchScratch(custody); err != nil {
				t.Fatal(err)
			}
			var starts atomic.Int32
			runtime := &launchScratchRuntime{custody: custody, Runtime: scratchFixtureRuntime{start: func(agentsessions.StartOptions) (agentsessions.Session, error) {
				starts.Add(1)
				return nil, errors.New("synthetic entered outcome unknown")
			}}}
			rig.service.Manager = agentsessions.NewManager(stateSinkAdapter{db: rig.service.Store}).WithEventSink(scratchFixtureEvents(func(event agentsessions.LifecycleEvent) {
				if event.To != agentsessions.StateLaunching {
					return
				}
				switch scenario {
				case "changed-row":
					if _, err := rig.service.Store.DB().Exec("UPDATE sessions SET intent='fork' WHERE id=?", rig.row.ID); err != nil {
						t.Fatal(err)
					}
				case "foreign-native-stamp":
					if err := rig.service.Store.SetNativeStateRoot(context.Background(), rig.row.ID, t.TempDir()); err != nil {
						t.Fatal(err)
					}
				case "root-substitution":
					if err := os.Rename(custody.allocation.Root, custody.allocation.Root+"-retained"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(custody.allocation.Root, 0o700); err != nil {
						t.Fatal(err)
					}
				case "holder-change":
					rig.unlock()
					rig.unlock = nil
				}
			}))
			if err := rig.service.Manager.Start(context.Background(), agentsessions.StartRequest{ID: rig.row.ID, Runtime: runtime}); err == nil {
				t.Fatal("fixture unexpectedly started")
			}
			rig.service.releaseScratchBeforeEntry(custody, runtime)
			if scenario == "unknown-start" {
				rig.service.releaseScratchAfterCompletion(context.Background(), custody) // Lookup error is not completion.
				if starts.Load() != 1 || !runtime.entered.Load() || scratchIsClosed(custody) || !rig.service.holdsLaunchScratch(rig.row.ID) {
					t.Fatal("entered unknown outcome lost custody")
				}
			} else if starts.Load() != 0 || runtime.entered.Load() || !scratchIsClosed(custody) || rig.service.holdsLaunchScratch(rig.row.ID) {
				t.Fatal("confirmed pre-entry refusal did not release handles")
			}
		})
	}
}

func TestLaunchScratchEscrowNeverOverwritesPriorCustody(t *testing.T) {
	rig := newScratchAdmissionRig(t)
	op := uuid.NewString()
	first, second := rig.admit(t, op), rig.admit(t, op)
	if err := rig.service.escrowLaunchScratch(first); err != nil {
		t.Fatal(err)
	}
	if err := rig.service.escrowLaunchScratch(second); !errors.Is(err, store.ErrWorkspaceAllocationConflict) {
		t.Fatalf("duplicate escrow: %v", err)
	}
	if scratchIsClosed(first) || !scratchIsClosed(second) || !rig.service.holdsLaunchScratch(rig.row.ID) {
		t.Fatal("duplicate escrow dropped original handles")
	}
}

func TestLaunchScratchServiceEnvironmentAndUnknownStart(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion", true: "unknown-entry"}[unknown], func(t *testing.T) {
			rig := newScratchAdmissionRig(t)
			rig.unlock()
			rig.unlock = nil
			rig.plan.RuntimeKind = config.RuntimeKindACPStdio
			rig.plan.ProviderBrand = "copilot"
			rig.plan.Command = "synthetic-never-executed"
			rig.plan.RepoRoot = rig.plan.WorkRoot
			rig.plan.Env["TMPDIR"] = "accepted-caller-temp"
			rig.plan.Env["CODEX_HOME"] = "accepted-caller-home"
			raw, err := json.Marshal(rig.plan)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rig.service.Store.DB().Exec("UPDATE launch_plans SET plan_json=? WHERE session_id=?", string(raw), rig.row.ID); err != nil {
				t.Fatal(err)
			}
			rig.service.CatalogRoot = t.TempDir()
			rig.service.Catalog = &config.Catalog{Global: config.Global{Version: "test"}}
			rig.service.Bus = events.NewBus(events.BusOptions{Persister: rig.service.Store})
			rig.service.Manager = agentsessions.NewManager(stateSinkAdapter{db: rig.service.Store})
			original := &scratchFixtureSession{strictNativeCoauthorSession: &strictNativeCoauthorSession{recoveryFakeSession: &recoveryFakeSession{done: make(chan struct{})}}}
			t.Cleanup(func() { _ = original.Stop(context.Background()) })
			var options agentsessions.StartOptions
			rig.service.factories = map[string]RuntimeFactory{rig.plan.ProviderID: func(*launch.Plan) (agentsessions.Runtime, error) {
				return scratchFixtureRuntime{kind: "acp", start: func(opts agentsessions.StartOptions) (agentsessions.Session, error) {
					options = opts
					if _, err := rig.service.Store.GetSession(rig.row.ID); err != nil {
						t.Fatal(err)
					}
					if unknown {
						return nil, errors.New("synthetic entered outcome unknown")
					}
					return original, nil
				}}, nil
			}}
			_, startErr := rig.service.LaunchSessionWithContext(context.Background(), rig.row.ID)
			if (startErr != nil) != unknown {
				t.Fatalf("launch error: %v", startErr)
			}
			allocation, err := rig.service.Store.WorkspaceScratchAllocation(context.Background(), rig.row.ID)
			if err != nil || allocation.Status != "admitted" {
				t.Fatalf("receipt: %+v %v", allocation, err)
			}
			var temp, home string
			for _, env := range options.Env {
				if strings.HasPrefix(env, "TMPDIR=") {
					temp = strings.TrimPrefix(env, "TMPDIR=")
				}
				if strings.HasPrefix(env, "CODEX_HOME=") {
					home = strings.TrimPrefix(env, "CODEX_HOME=")
				}
			}
			if temp != allocation.Root || home != "accepted-caller-home" || options.Workdir != rig.plan.WorkRoot {
				t.Fatal("runtime allocation changed credential home or work root")
			}
			stored, err := rig.service.Store.GetLaunchPlan(rig.row.ID)
			if err != nil || stored.Env["TMPDIR"] != "accepted-caller-temp" || stored.WorkRoot != rig.plan.WorkRoot {
				t.Fatal("runtime environment mutated accepted plan")
			}
			if unknown {
				if !rig.service.holdsLaunchScratch(rig.row.ID) {
					t.Fatal("unknown start lost escrow")
				}
				return
			}
			if err := original.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			for rig.service.holdsLaunchScratch(rig.row.ID) && ctx.Err() == nil {
				runtime.Gosched()
			}
			if rig.service.holdsLaunchScratch(rig.row.ID) || original.waits.Load() != 1 {
				t.Fatal("authoritative completion did not release custody exactly once")
			}
			if _, err := os.Stat(allocation.Root); err != nil {
				t.Fatal("completion deleted retained scratch")
			}
		})
	}
}

func TestLaunchScratchWaitCancellationAndShimCustodyRemainRetained(t *testing.T) {
	for _, scenario := range []string{"canceled-observer", "tracked-shim"} {
		t.Run(scenario, func(t *testing.T) {
			rig := newScratchAdmissionRig(t)
			custody := rig.admit(t, uuid.NewString())
			if err := rig.service.escrowLaunchScratch(custody); err != nil {
				t.Fatal(err)
			}
			original := &scratchFixtureSession{strictNativeCoauthorSession: &strictNativeCoauthorSession{recoveryFakeSession: &recoveryFakeSession{done: make(chan struct{})}}}
			t.Cleanup(func() { _ = original.Stop(context.Background()) })
			rig.service.Manager = agentsessions.NewManager(stateSinkAdapter{db: rig.service.Store})
			wrapped := &launchScratchRuntime{custody: custody, Runtime: scratchFixtureRuntime{start: func(agentsessions.StartOptions) (agentsessions.Session, error) { return original, nil }}}
			if err := rig.service.Manager.Start(context.Background(), agentsessions.StartRequest{ID: rig.row.ID, Runtime: wrapped}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			if scenario == "canceled-observer" {
				cancel()
			} else {
				defer cancel()
				if err := rig.service.Store.UpsertSessionShim(ctx, store.SessionShimRow{SessionID: rig.row.ID, ShimKey: "synthetic-key", HostBackend: "detached", SocketPath: filepath.Join(rig.row.Workspace, "socket"), DescriptorPath: filepath.Join(rig.row.Workspace, "descriptor"), Runtime: "fixture", RuntimeGeneration: 1, BootGeneration: "synthetic-boot"}); err != nil {
					t.Fatal(err)
				}
				if err := original.Stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			rig.service.releaseScratchAfterCompletion(ctx, custody)
			if scratchIsClosed(custody) || !rig.service.holdsLaunchScratch(rig.row.ID) {
				t.Fatal("observer cancellation or bridge completion erased custody")
			}
		})
	}
}

func newScratchAdmissionRig(t *testing.T) *scratchAdmissionRig {
	t.Helper()
	control := t.TempDir()
	db, err := store.Open(filepath.Join(control, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	plan := &launch.Plan{LaunchID: "launch", ProviderID: "codex", ProjectID: "project", WorkRoot: t.TempDir(), Env: map[string]string{"KEEP": "accepted"}}
	row := store.SessionRow{ID: "scratch-session", LaunchID: "launch", ProviderID: "codex", ProjectID: "project", State: "created", Workspace: t.TempDir()}
	if err := db.CreateSession(row, plan); err != nil {
		t.Fatal(err)
	}
	canonical, err := db.GetSession(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = db.GetLaunchPlan(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: db}
	unlock, err := svc.lockSessionLaunch(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := db.WorkspaceLaunchDecision(context.Background(), canonical.ID)
	if err != nil {
		t.Fatal(err)
	}
	rig := &scratchAdmissionRig{service: svc, row: canonical, decision: decision, plan: plan, control: control, unlock: unlock}
	t.Cleanup(func() {
		if rig.unlock != nil {
			rig.unlock()
		}
	})
	return rig
}

func (r *scratchAdmissionRig) admit(t *testing.T, operation string) *launchScratchCustody {
	t.Helper()
	custody, err := r.service.admitLaunchScratch(context.Background(), r.decision, r.plan, operation, []string{r.control})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = custody.Close() })
	return custody
}

func TestLaunchScratchAdmittedRetryPreservesAcceptedPlan(t *testing.T) {
	rig := newScratchAdmissionRig(t)
	op := uuid.NewString()
	first := rig.admit(t, op)
	second := rig.admit(t, op)
	if first.allocation != second.allocation || first.allocation.Status != "admitted" {
		t.Fatal("same operation did not retain identity")
	}
	if err := os.WriteFile(filepath.Join(first.allocation.Root, "retained-output"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(first.allocation.Root, "retained-output")); err != nil {
		t.Fatalf("close deleted allocation: %v", err)
	}
	stored, err := rig.service.Store.GetLaunchPlan(rig.row.ID)
	if err != nil || stored.WorkRoot != rig.plan.WorkRoot || stored.Env["KEEP"] != "accepted" || stored.Env["TMPDIR"] != "" || stored.Env["CODEX_HOME"] != "" {
		t.Fatalf("scratch changed accepted plan: %+v err=%v", stored, err)
	}
}

func TestLaunchScratchRetainsConfiguredControlAliasIdentity(t *testing.T) {
	rig := newScratchAdmissionRig(t)
	catalog := t.TempDir()
	alias := filepath.Join(t.TempDir(), "catalog")
	if err := os.Symlink(catalog, alias); err != nil {
		t.Fatal(err)
	}
	rig.service.CatalogRoot = alias
	roots, err := rig.service.launchScratchControlRoots(context.Background(), rig.row.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	custody, err := rig.service.admitLaunchScratch(context.Background(), rig.decision, rig.plan, uuid.NewString(), roots)
	if err != nil {
		t.Fatalf("accepted daemon control alias refused: %v", err)
	}
	t.Cleanup(func() { _ = custody.Close() })
	replacement := alias + "-replacement"
	if err := os.Symlink(t.TempDir(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, alias); err != nil {
		t.Fatal(err)
	}
	if err := custody.Validate(context.Background()); err == nil {
		t.Fatal("retargeted control alias retained admission")
	}
}

func TestLaunchScratchUnusableUnrelatedProjectLayer(t *testing.T) {
	for _, withinWorkspace := range []bool{false, true} {
		t.Run(fmt.Sprintf("within-workspace=%t", withinWorkspace), func(t *testing.T) {
			rig := newScratchAdmissionRig(t)
			base := t.TempDir()
			if withinWorkspace {
				base = rig.row.Workspace
			}
			file := filepath.Join(base, "not-a-directory")
			if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			rig.service.CatalogRoot = t.TempDir()
			rig.service.Catalog = &config.Catalog{Projects: map[string]config.Project{
				"broken": {ID: "broken", RepoRoot: filepath.Join(file, "child")},
			}}
			roots, err := rig.service.launchScratchControlRoots(context.Background(), rig.row.Workspace)
			if withinWorkspace {
				if !errors.Is(err, store.ErrWorkspaceAllocationConflict) {
					t.Fatalf("unresolved layer inside workspace accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unrelated nonexistent layer refused allocation: %v", err)
			}
			custody, err := rig.service.admitLaunchScratch(context.Background(), rig.decision, rig.plan, uuid.NewString(), roots)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = custody.Close() })
		})
	}
}

func TestLaunchScratchRefusesChangedCustody(t *testing.T) {
	for _, change := range []string{"root", "parent", "protected-root", "state", "plan", "gate", "permissions"} {
		t.Run(change, func(t *testing.T) {
			rig := newScratchAdmissionRig(t)
			custody := rig.admit(t, uuid.NewString())
			switch change {
			case "root", "parent":
				target := custody.allocation.Root
				if change == "parent" {
					target = rig.row.Workspace
				}
				if err := os.Rename(target, target+"-retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
			case "state":
				if err := rig.service.Store.UpdateSessionState(rig.row.ID, "launching", 0, nil); err != nil {
					t.Fatal(err)
				}
			case "protected-root":
				if err := os.Rename(rig.control, rig.control+"-retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(rig.control, 0o700); err != nil {
					t.Fatal(err)
				}
			case "plan":
				if _, err := rig.service.Store.DB().Exec("UPDATE launch_plans SET plan_json='{}' WHERE session_id=?", rig.row.ID); err != nil {
					t.Fatal(err)
				}
			case "gate":
				rig.unlock()
				rig.unlock = nil
			case "permissions":
				if err := os.Chmod(custody.allocation.Root, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := custody.Validate(context.Background()); err == nil {
				t.Fatal("changed custody accepted")
			}
		})
	}
}

func TestLaunchScratchAdmittedReplayRefusesSubstitution(t *testing.T) {
	rig := newScratchAdmissionRig(t)
	op := uuid.NewString()
	custody := rig.admit(t, op)
	root := custody.allocation.Root
	if err := custody.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.service.admitLaunchScratch(context.Background(), rig.decision, rig.plan, op, []string{rig.control}); !errors.Is(err, store.ErrWorkspaceAllocationConflict) {
		t.Fatalf("substituted replay: %v", err)
	}
	if _, err := os.Stat(root + "-retained"); err != nil {
		t.Fatal("original allocation lost")
	}
}

func TestLaunchScratchRetainsForeignAndPartialAllocations(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign", true: "reserved-partial"}[partial], func(t *testing.T) {
			rig := newScratchAdmissionRig(t)
			op := uuid.NewString()
			root := filepath.Join(rig.row.Workspace, ".tether-scratch-"+op)
			if partial {
				if _, _, err := rig.service.Store.ReservePreparedWorkspaceScratch(context.Background(), rig.decision, op); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			retained := filepath.Join(root, "owned-by-other")
			if err := os.WriteFile(retained, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := rig.service.admitLaunchScratch(context.Background(), rig.decision, rig.plan, op, []string{rig.control}); err == nil {
				t.Fatal("unknown existing scratch adopted")
			}
			if _, err := os.Stat(retained); err != nil {
				t.Fatal("unknown scratch removed")
			}
			allocation, replay, err := rig.service.Store.ReservePreparedWorkspaceScratch(context.Background(), rig.decision, op)
			if err != nil || !replay || allocation.Status != "reserved" {
				t.Fatalf("partial reservation lost: %+v %v %v", allocation, replay, err)
			}
		})
	}
}

func TestLaunchScratchRefusesProtectedRootAndUnheldGate(t *testing.T) {
	rig := newScratchAdmissionRig(t)
	if _, err := rig.service.admitLaunchScratch(context.Background(), rig.decision, rig.plan, uuid.NewString(), []string{rig.row.Workspace}); !errors.Is(err, store.ErrWorkspaceAllocationConflict) {
		t.Fatalf("protected root: %v", err)
	}
	rig.unlock()
	rig.unlock = nil
	if _, err := rig.service.admitLaunchScratch(context.Background(), rig.decision, rig.plan, uuid.NewString(), []string{rig.control}); err == nil {
		t.Fatal("unheld launch gate admitted scratch")
	}
}

func TestLaunchScratchNeverUsesExistingNativeHome(t *testing.T) {
	rig := newScratchAdmissionRig(t)
	rig.plan.NativeStateRoot = rig.row.Workspace
	if err := rig.service.Store.SetNativeStateRoot(context.Background(), rig.row.ID, rig.row.Workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.service.admitLaunchScratch(context.Background(), rig.decision, rig.plan, uuid.NewString(), []string{rig.control}); !errors.Is(err, store.ErrWorkspaceAllocationConflict) {
		t.Fatalf("existing native home scratch: %v", err)
	}
	entries, err := os.ReadDir(rig.row.Workspace)
	if err != nil || len(entries) != 0 {
		t.Fatalf("native home was changed: %v err=%v", entries, err)
	}
}
