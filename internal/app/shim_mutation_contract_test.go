//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	gop "github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/workspace"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-runner/runner"
	"github.com/hollis-labs/go-sandbox/sandbox"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/shimbridge"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
)

type shimCapsRuntime struct {
	agentsessions.Runtime
	caps agentsessions.Capabilities
}

func (r shimCapsRuntime) Caps() agentsessions.Capabilities { return r.caps }

func TestShimUnsupportedRuntimeClauses(t *testing.T) {
	for _, why := range []string{"streaming", "prepared", "launch", "mode", "files", "supervisor"} {
		t.Run(why, func(t *testing.T) {
			f := shimFixture(t)
			switch why {
			case "streaming":
				f.req.Runtime = shimCapsRuntime{Runtime: f.req.Runtime}
			case "prepared":
				f.req.Options.PreparedExecution = &agentlaunch.PreparedExecution{}
			case "launch":
				f.req.Options.Launch = nil
			case "mode":
				f.req.Options.Launch.Convention.Mode = "unsupported-mode"
			case "files":
				f.req.Options.ExtraFiles = []*os.File{os.Stdout}
			case "supervisor":
				f.req.Options.Supervisor = &agentsessions.SupervisorOptions{}
			}
			f.svc.shimHosting.prepare = func(shim.Launch, *sandbox.ResolvedAccessPolicy, runner.ResourceLimits) (shim.Launch, func(), error) {
				t.Fatal("unsupported request reached policy preparation")
				return shim.Launch{}, nil, nil
			}
			got, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
			if err != nil || !reflect.DeepEqual(got, f.req) {
				t.Fatalf("unsupported request changed: %v", err)
			}
			if _, err := f.svc.Store.SessionShim(context.Background(), f.req.ID); !errors.Is(err, store.ErrSessionShimNotFound) {
				t.Fatal("unsupported request recorded placement")
			}
			statuses := shimStatusEvents(t, f)
			if len(statuses) != 1 || statuses[0].Reason != "unsupported_runtime" {
				t.Fatalf("wrong refusal: %+v", statuses)
			}
		})
	}
}

func TestShimBridgeOptionsClearProviderConfinementAndBoot(t *testing.T) {
	for _, attach := range []bool{false, true} {
		opts := agentsessions.StartOptions{Profile: sandbox.Profile{ID: "private"}, SandboxPolicy: &sandbox.ResolvedAccessPolicy{}, PreparedExecution: &agentlaunch.PreparedExecution{}, ProtectedPaths: []string{"/private"}, DenyGUILaunch: true, Supervisor: &agentsessions.SupervisorOptions{}, ResourceLimits: &agentsessions.ResourceLimits{}, Env: []string{"PROVIDER_SECRET=sentinel"}, ExtraArgs: []string{"provider-arg"}, BootPrompt: "boot", BootContent: "content", BootMode: "stdin", FirstTurnPayload: []byte("turn"), AutoFireFirstTurn: true}
		got := shimBridgeOptions(opts, []string{"bridge"}, shimhost.Receipt{DescriptorPath: "/descriptor", Journal: "journal"}, attach)
		if got.Profile.ID != "" || got.SandboxPolicy != nil || got.PreparedExecution != nil || got.ProtectedPaths != nil || got.DenyGUILaunch || got.ResourceLimits != nil || got.Supervisor != nil || got.ExtraArgs != nil || !reflect.DeepEqual(got.Env, trustedShimEnv()) {
			t.Fatalf("bridge retained provider authority: %+v", got)
		}
		if attach && (got.AutoFireFirstTurn || len(got.FirstTurnPayload) > 0 || got.BootPrompt != "" || got.BootContent != "" || got.BootMode != "none") {
			t.Fatal("attach replayed boot")
		}
		if attach {
			args, err := got.Launch.TurnArgv(gop.TurnInput{})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(args, "--takeover") {
				t.Fatal("attach omitted takeover")
			}
		}
	}
}

func TestShimTrustedEnvironmentIsAllowlisted(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_API_KEY", "TETHER_TOKEN", "PROVIDER_SECRET"} {
		t.Setenv(key, "sentinel")
	}
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "XDG_RUNTIME_DIR": true, "DBUS_SESSION_BUS_ADDRESS": true}
	for _, entry := range trustedShimEnv() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !allowed[key] {
			t.Fatalf("trusted environment leaked key %q", key)
		}
	}
}

func TestShimReceiptIdentityClauses(t *testing.T) {
	for _, field := range []string{"session", "key", "generation", "backend", "descriptor", "socket", "unit", "journal"} {
		t.Run(field, func(t *testing.T) {
			f := shimFixture(t)
			r := f.svc.shimHosting.provider.PlacementIdentity(f.req.ID+":1", shimLaunchIdentity(f))
			r.Journal = "journal"
			if err := shimhost.PrivateDir(filepath.Dir(r.DescriptorPath)); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.persistShim(context.Background(), f.req.ID, "claude", "boot", r); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.Store.UpdateSessionState(f.req.ID, "running", 0, nil); err != nil {
				t.Fatal(err)
			}
			canonical := r
			switch field {
			case "session":
				r.Session = "other"
			case "key":
				r.OperationKey = "other"
			case "generation":
				r.Generation++
			case "backend":
				r.Backend = "systemd-user"
			case "descriptor":
				r.DescriptorPath += "other"
			case "socket":
				r.SocketPath += "other"
			case "unit":
				r.UnitName = "other.service"
			case "journal":
				r.Journal = "other"
			}
			if err := shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(canonical.DescriptorPath), "placement.json"), r); err != nil {
				t.Fatal(err)
			}
			row, err := f.svc.Store.SessionShim(context.Background(), f.req.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loadShimReceipt(row); err == nil || shimFailureCode(err) != "identity_mismatch" {
				t.Fatalf("accepted %s mismatch: %v", field, err)
			}
			f.svc.shimHosting.stop = func(context.Context, shimhost.Receipt) error { t.Fatal("identity mismatch reached host"); return nil }
			if err := f.svc.StopSession(f.req.ID); err == nil || shimFailureCode(err) != "identity_mismatch" {
				t.Fatalf("stop accepted %s: %v", field, err)
			}
			// No process exists; the fixture's host cleanup must not load the deliberately
			// invalid receipt after this contract test.
			if err := shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(canonical.DescriptorPath), "placement.json"), canonical); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShimCheckpointClauses(t *testing.T) {
	for _, field := range []string{"session", "instance", "generation", "journal", "counter", "cursor", "partial", "init", "exit", "corrupt"} {
		t.Run(field, func(t *testing.T) {
			f := shimFixture(t)
			r := f.svc.shimHosting.provider.PlacementIdentity(f.req.ID+":1", shimLaunchIdentity(f))
			r.Journal = "journal"
			if err := shimhost.PrivateDir(filepath.Dir(r.DescriptorPath)); err != nil {
				t.Fatal(err)
			}
			cp := shimbridge.Checkpoint{Session: r.Session, Instance: r.Instance, Generation: r.Generation, Journal: r.Journal}
			want := "journal_mismatch"
			switch field {
			case "session":
				cp.Session = "other"
				want = "identity_mismatch"
			case "instance":
				cp.Instance = "other"
				want = "identity_mismatch"
			case "generation":
				cp.Generation++
				want = "identity_mismatch"
			case "journal":
				cp.Journal = "other"
			case "counter":
				cp.Journal = ""
				cp.Counter = 1
			case "cursor":
				cp.Journal = ""
				cp.Cursor = "journal:1"
			case "partial":
				cp.Journal = ""
				cp.Partial = []byte("carry")
			case "init":
				cp.Journal = ""
				cp.Init = []byte("init")
			case "exit":
				cp.Journal = ""
				cp.Exit = &shim.Exit{}
			case "corrupt":
				want = "outcome_unknown"
			}
			path := filepath.Join(filepath.Dir(r.DescriptorPath), "bridge.json")
			if err := shimhost.WritePrivateJSON(path, cp); err != nil {
				t.Fatal(err)
			}
			if field == "corrupt" {
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := initializeShimCheckpoint(r); err == nil || shimFailureCode(err) != want {
				t.Fatalf("checkpoint %s accepted: %v", field, err)
			}
		})
	}
}

func TestShimIntentDurableBeforePlacementAndUnattemptedFallback(t *testing.T) {
	f := shimFixture(t)
	cleaned := 0
	prepare := f.svc.shimHosting.prepare
	f.svc.shimHosting.prepare = func(spec shim.Launch, policy *sandbox.ResolvedAccessPolicy, limits runner.ResourceLimits) (shim.Launch, func(), error) {
		spec, cleanup, err := prepare(spec, policy, limits)
		return spec, func() { cleaned++; cleanup() }, err
	}
	f.svc.shimHosting.place = func(_ context.Context, key string, spec shim.Launch) (shimhost.Receipt, error) {
		row, err := f.svc.Store.SessionShim(context.Background(), f.req.ID)
		if err != nil || row.ShimKey != key || row.HostPID != 0 || row.ProviderPID != 0 {
			t.Fatalf("placement preceded intent: %+v %v", row, err)
		}
		session, err := f.svc.Store.GetSession(f.req.ID)
		if err != nil || session.State != "launching" {
			t.Fatal("placement preceded recoverable state")
		}
		return shimhost.Receipt{}, &shimhost.Failure{Code: "placement_busy"}
	}
	req, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
	if err != nil || req.Runtime != f.req.Runtime || cleaned != 1 {
		t.Fatalf("unattempted fallback: %v cleanup=%d", err, cleaned)
	}
	row, _ := f.svc.Store.GetSession(f.req.ID)
	if row.State != "created" {
		t.Fatalf("fallback state=%s", row.State)
	}
	if _, err := f.svc.Store.SessionShim(context.Background(), f.req.ID); !errors.Is(err, store.ErrSessionShimNotFound) {
		t.Fatal("fallback kept intent")
	}
}

func TestShimServiceScopeRefusedAndCleanedBeforePlacement(t *testing.T) {
	f := shimFixture(t)
	cleaned := 0
	f.svc.shimHosting.prepare = func(spec shim.Launch, _ *sandbox.ResolvedAccessPolicy, _ runner.ResourceLimits) (shim.Launch, func(), error) {
		spec.Argv = []string{"systemd-run"}
		return spec, func() { cleaned++ }, nil
	}
	f.svc.shimHosting.place = func(context.Context, string, shim.Launch) (shimhost.Receipt, error) {
		t.Fatal("service scope placed")
		return shimhost.Receipt{}, nil
	}
	req, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
	if err != nil || req.Runtime != f.req.Runtime || cleaned != 1 {
		t.Fatalf("scope fallback: %v cleanup=%d", err, cleaned)
	}
	if _, err := f.svc.Store.SessionShim(context.Background(), f.req.ID); !errors.Is(err, store.ErrSessionShimNotFound) {
		t.Fatal("scope persisted intent")
	}
}

func TestShimLiveManagerPreventsReinspection(t *testing.T) {
	f := shimFixture(t)
	f.start(t)
	f.svc.shimHosting.inspect = func(context.Context, shimhost.Receipt) (shimhost.Inspection, error) {
		t.Fatal("live bridge re-inspected")
		return shimhost.Inspection{}, nil
	}
	f.svc.ReconcileStaleState()
}

func TestShimDrainNeverInspectsOrStopsProvider(t *testing.T) {
	f := shimFixture(t)
	f.start(t)
	f.svc.shimHosting.inspect = func(context.Context, shimhost.Receipt) (shimhost.Inspection, error) {
		t.Error("drain inspected provider")
		return shimhost.Inspection{}, context.DeadlineExceeded
	}
	f.svc.shimHosting.stop = func(context.Context, shimhost.Receipt) error { t.Error("drain stopped provider"); return nil }
	// Race instrumentation adds cost to the terminal shim-row checks.
	ctx, cancel := context.WithTimeout(context.Background(), shimFixtureBudget)
	defer cancel()
	shimDrainWithDiagnostics(ctx, t, f)
	statuses := shimStatusEvents(t, f)
	if len(statuses) != 1 || statuses[0].Reason != ShutdownStopReason {
		t.Fatalf("drain reason=%+v", statuses)
	}
	shimAwait(t, "watcher entries released", func() bool {
		_, waiting := f.svc.shimBindingWait.Load(f.req.ID)
		_, draining := f.svc.shimDraining.Load(f.req.ID)
		return !waiting && !draining
	})
}

func shimRealLaunchFixture(t *testing.T) *shimAppFixture {
	t.Helper()
	f := shimFixture(t)
	f.req.ID = "real-shim-session"
	f.plan.RepoRoot = t.TempDir()
	f.plan.WorkRoot = f.plan.RepoRoot
	f.plan.WriteHome = t.TempDir()
	f.plan.WorkspaceMode = "shared"
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(f.plan.RepoRoot, "fake-provider")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run='^TestShimLaunchProcess$' -- provider \"$@\"\n"
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.plan.Command = fake
	f.plan.Args = nil
	f.plan.PermissionMode = config.PermissionModeBypass
	ws, err := workspace.Create(f.plan.WriteHome, f.req.ID, f.plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Store.CreateSession(store.SessionRow{ID: f.req.ID, LaunchID: f.plan.LaunchID, ProjectID: f.plan.ProjectID, LogicalAgentID: f.plan.LogicalAgentID, ProviderID: f.plan.ProviderID, ProviderKind: "cli", Workspace: ws.Root, State: "created"}, f.plan); err != nil {
		t.Fatal(err)
	}
	f.svc.CatalogRoot = t.TempDir()
	f.svc.factories = map[string]RuntimeFactory{f.plan.ProviderID: func(*launch.Plan) (agentsessions.Runtime, error) { return f.req.Runtime, nil }}
	return f
}

func TestShimRealLaunchWiringAndCredentialRetention(t *testing.T) {
	for _, mode := range []string{"direct", "shim", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			f := shimRealLaunchFixture(t)
			if mode == "direct" {
				t.Setenv(EnvLaunchHost, "direct")
			}
			placed := 0
			f.svc.shimHosting.place = func(ctx context.Context, key string, spec shim.Launch) (shimhost.Receipt, error) {
				placed++
				r, err := f.svc.shimHosting.provider.Place(ctx, key, spec)
				if err == nil && mode == "unknown" {
					return r, &shimhost.Failure{Code: "outcome_unknown"}
				}
				return r, err
			}
			_, err := f.svc.LaunchSession(f.req.ID)
			if mode == "unknown" {
				if err == nil || shimFailureCode(err) != "outcome_unknown" {
					t.Fatalf("unknown launch started direct provider: %v", err)
				}
				if _, live := f.svc.Manager.Get(f.req.ID); live {
					t.Fatal("uncertain launch started a runtime")
				}
				var active int
				if err := f.svc.Store.DB().QueryRow(`SELECT COUNT(*) FROM principals WHERE session_id=? AND revoked_at IS NULL`, f.req.ID).Scan(&active); err != nil || active != 1 {
					t.Fatalf("retained placement lost credential: active=%d %v", active, err)
				}
				shimRow, e := f.svc.Store.SessionShim(context.Background(), f.req.ID)
				if e != nil {
					t.Fatal(e)
				}
				receipt, e := loadShimReceipt(shimRow)
				if e != nil {
					t.Fatal(e)
				}
				var spec shim.Launch
				if e := shimhost.ReadPrivateJSON(receipt.DescriptorPath, shim.MaxFrame, &spec); e != nil {
					t.Fatal(e)
				}
				token := ""
				for _, entry := range spec.Env {
					key, value, _ := strings.Cut(entry, "=")
					if key == "TETHER_TOKEN" {
						token = value
					}
				}
				principal, e := identity.NewStore(f.svc.Store.DB()).Verify(context.Background(), token)
				if e != nil || principal.SessionID != f.req.ID {
					t.Fatal("retained provider credential no longer verifies")
				}

			} else if err != nil {
				t.Fatalf("real %s launch: %v", mode, err)
			}
			_, rowErr := f.svc.Store.SessionShim(context.Background(), f.req.ID)
			if mode == "direct" {
				if placed != 0 || !errors.Is(rowErr, store.ErrSessionShimNotFound) {
					t.Fatalf("default launch submitted shim: %d %v", placed, rowErr)
				}
			} else if placed != 1 || rowErr != nil {
				t.Fatalf("shim flag not wired: %d %v", placed, rowErr)
			}
		})
	}
}

func TestShimPublishedResultUUIDAndReplay(t *testing.T) {
	f := shimFixture(t)
	f.start(t)
	if err := f.svc.SendTurn(context.Background(), f.req.ID, "uuid"); err != nil {
		t.Fatal(err)
	}
	shimAwait(t, "published UUID", func() bool { return len(outputEvents(t, f.svc)) == 1 })
	ev := outputEvents(t, f.svc)[0]
	if ev.ProviderResultID != "result-uuid" {
		t.Fatalf("published UUID lost: %+v", ev)
	}
	adapter := &shimClaudeAdapter{ClaudeAdapter: gop.NewClaudeAdapterStreamingStdio(), service: f.svc, sessionID: f.req.ID}
	line := []byte(`{"type":"result","subtype":"success","uuid":"result-uuid","result":"reply:uuid"}`)
	legacy, err := adapter.ParseLine(line)
	if err != nil {
		t.Fatal(err)
	}
	typed, err := adapter.ParseLineEvents(line)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 0 || len(typed) != 0 || len(outputEvents(t, f.svc)) != 1 {
		t.Fatal("durably published result replayed")
	}
	// A non-result UUID must never suppress assistant text.
	line = []byte(`{"type":"assistant","uuid":"result-uuid","message":{"content":[{"type":"text","text":"new text"}]}}`)
	if _, err := adapter.ParseLine(line); err != nil {
		t.Fatal(err)
	}
	typed, err = adapter.ParseLineEvents(line)
	if err != nil || len(typed) == 0 {
		t.Fatalf("assistant UUID suppressed output: %v", err)
	}
}

func TestShimEmptyResultIdentityIsNeverSuppressed(t *testing.T) {
	f := shimFixture(t)
	payload, _ := json.Marshal(events.TurnOutputEvent{SessionID: f.req.ID, TurnID: "old", Text: "old"})
	if err := f.svc.Bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: f.req.ID, Kind: events.KindSessionTurnOutput, PayloadJSON: string(payload)}); err != nil {
		t.Fatal(err)
	}
	adapter := &shimClaudeAdapter{ClaudeAdapter: gop.NewClaudeAdapterStreamingStdio(), service: f.svc, sessionID: f.req.ID}
	line := []byte(`{"type":"result","subtype":"success","result":"new text"}`)
	if _, err := adapter.ParseLine(line); err != nil {
		t.Fatal(err)
	}
	typed, err := adapter.ParseLineEvents(line)
	if err != nil || len(typed) == 0 {
		t.Fatalf("identity-free result suppressed: %v", err)
	}
}

func TestShimCleanupLifetime(t *testing.T) {
	for _, end := range []string{"stop", "exit", "detach"} {
		t.Run(end, func(t *testing.T) {
			f := shimFixture(t)
			var cleaned atomic.Int32
			prepare := f.svc.shimHosting.prepare
			f.svc.shimHosting.prepare = func(spec shim.Launch, policy *sandbox.ResolvedAccessPolicy, limits runner.ResourceLimits) (shim.Launch, func(), error) {
				spec, cleanup, err := prepare(spec, policy, limits)
				return spec, func() { cleaned.Add(1); cleanup() }, err
			}
			f.start(t)
			if cleaned.Load() != 0 {
				t.Fatal("cleanup preceded provider exit")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			switch end {
			case "stop":
				if err := f.svc.StopSession(f.req.ID); err != nil {
					t.Fatal(err)
				}
			case "exit":
				if err := f.svc.SendTurn(ctx, f.req.ID, "exit"); err != nil {
					t.Fatal(err)
				}
			case "detach":
				if err := f.svc.DrainSessions(ctx); err != nil {
					t.Fatal(err)
				}
				if cleaned.Load() != 0 {
					t.Fatal("detach removed provider resources")
				}
				if err := f.svc.StopSession(f.req.ID); err != nil {
					t.Fatal(err)
				}
			}
			if end == "exit" {
				shimAwait(t, "provider terminal outcome", func() bool { row, _ := f.svc.Store.GetSession(f.req.ID); return row.State == "failed" })
				if err := f.svc.waitShimBinding(ctx, f.req.ID); err != nil {
					t.Fatal(err)
				}
			}
			if cleaned.Load() != 1 {
				t.Fatalf("retired provider cleanup count=%d", cleaned.Load())
			}
		})
	}
}

func TestShimMultiResultReplayUsesEveryPublishedUUID(t *testing.T) {
	f := shimFixture(t)
	f.start(t)
	for i, text := range []string{"first", "second"} {
		if err := f.svc.SendTurn(context.Background(), f.req.ID, text); err != nil {
			t.Fatal(err)
		}
		shimAwait(t, "published replay boundary", func() bool { return len(outputEvents(t, f.svc)) == i+1 })
	}
	adapter := &shimClaudeAdapter{ClaudeAdapter: gop.NewClaudeAdapterStreamingStdio(), service: f.svc, sessionID: f.req.ID}
	for _, uuid := range []string{"result-first", "result-second"} {
		line, _ := json.Marshal(map[string]string{"type": "result", "subtype": "success", "uuid": uuid, "result": "same text"})
		legacy, err := adapter.ParseLine(line)
		if err != nil {
			t.Fatal(err)
		}
		typed, err := adapter.ParseLineEvents(line)
		if err != nil {
			t.Fatal(err)
		}
		if len(legacy) != 0 || len(typed) != 0 {
			t.Fatalf("older published result %s replayed", uuid)
		}
	}
}

func TestShimResolvedPolicyProtectionsAndRefusals(t *testing.T) {
	for _, mode := range []string{"required", "disabled", "loopback"} {
		t.Run(mode, func(t *testing.T) {
			f := shimFixture(t)
			work := t.TempDir()
			f.req.Options.Workdir = work
			request := sandbox.PolicyFromProfile(sandbox.Profile{ID: "resolved", HostFilesystem: true, Net: true, Subprocess: true}, work)
			request.FS.Write = append(request.FS.Write, sandbox.PathRef{Path: "/"})
			policy, err := sandbox.ResolveAccessPolicy(request)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "disabled" {
				policy.Mode = sandbox.ConfinementDisabled
			}
			if mode == "loopback" {
				policy.Network.LoopbackPorts = []int{8123}
			}
			f.req.Options.SandboxPolicy = &policy
			called := false
			f.svc.shimHosting.prepare = func(spec shim.Launch, p *sandbox.ResolvedAccessPolicy, _ runner.ResourceLimits) (shim.Launch, func(), error) {
				called = true
				root := filepath.Dir(f.svc.shimHosting.provider.SessionDir(f.req.ID))
				if !p.DenyUserServiceManager || p.AccessFor(root) != sandbox.AccessDenied || p.AccessFor(filepath.Dir(root)) != sandbox.AccessReadOnly {
					t.Fatal("resolved policy lost private-state protections")
				}
				return spec, func() {}, errors.New("probe complete")
			}
			f.svc.shimHosting.place = func(context.Context, string, shim.Launch) (shimhost.Receipt, error) {
				t.Fatal("unsupported policy reached placement")
				return shimhost.Receipt{}, nil
			}
			got, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
			if err != nil || got.Runtime != f.req.Runtime {
				t.Fatalf("policy refusal failed: %v", err)
			}
			if called != (mode == "required") {
				t.Fatalf("policy mode %s reached prepare=%v", mode, called)
			}
		})
	}
}

func TestShimOriginalStateCanMintPrincipal(t *testing.T) {
	for _, state := range []string{"created", "launching", "running", "detached"} {
		t.Run(state, func(t *testing.T) {
			svc := bindingHarness(t)
			if err := svc.Store.CreateSession(store.SessionRow{ID: "s", State: state}, &launch.Plan{}); err != nil {
				t.Fatal(err)
			}
			ids := identity.NewStore(svc.Store.DB())
			token, err := ids.Mint(context.Background(), identity.Principal{ID: "session-principal", Kind: "session", SessionID: "s"})
			if err != nil {
				t.Fatalf("existing %s mint changed: %v", state, err)
			}
			if _, err := ids.Verify(context.Background(), token); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShimStopPolicyCheckpointPrecedesSignals(t *testing.T) {
	f := shimFixture(t)
	f.start(t)
	if _, err := f.svc.UpdateLogicalAgentPolicy(agent.LogicalAgentPolicy{LogicalAgentID: "worker", CheckpointPolicy: agent.CheckpointPolicyOnStop}); err != nil {
		t.Fatal(err)
	}
	f.svc.shimHosting.stop = func(ctx context.Context, r shimhost.Receipt) error {
		rows, err := f.svc.Store.ListCheckpointsByLogicalAgent("worker")
		if err != nil || len(rows) != 1 || rows[0].SourceSessionID != f.req.ID {
			t.Fatalf("stop preceded checkpoint: %+v %v", rows, err)
		}
		return f.svc.shimHosting.provider.Stop(ctx, r)
	}
	if err := f.svc.StopSession(f.req.ID); err != nil {
		t.Fatal(err)
	}
}

func TestShimDetachedStopRevokesBindingImmediately(t *testing.T) {
	f := shimFixture(t)
	f.start(t)
	ids, token := recoveryCredential(t, f.svc, f.req.ID)
	// Race instrumentation adds cost to the terminal shim-row checks.
	ctx, cancel := context.WithTimeout(context.Background(), shimFixtureBudget)
	defer cancel()
	shimDrainWithDiagnostics(ctx, t, f)
	if _, err := currentWorkerBinding(f.svc); err != nil {
		t.Fatal("detach revoked binding")
	}
	if _, err := ids.Verify(ctx, token); err != nil {
		t.Fatal("detach revoked credential")
	}
	if err := f.svc.StopSession(f.req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := currentWorkerBinding(f.svc); !errors.Is(err, registry.ErrBindingNotFound) {
		t.Fatalf("detached stop left binding: %v", err)
	}
	if _, err := ids.Verify(ctx, token); err == nil {
		t.Fatal("detached stop left credential")
	}
}

func TestShimSettleGuardsNeverInspect(t *testing.T) {
	for _, why := range []string{"draining", "stopping"} {
		t.Run(why, func(t *testing.T) {
			f := shimFixture(t)
			r := f.svc.shimHosting.provider.PlacementIdentity(f.req.ID+":1", shimLaunchIdentity(f))
			if err := f.svc.persistShim(context.Background(), f.req.ID, "claude", "boot", r); err != nil {
				t.Fatal(err)
			}
			if err := shimhost.PrivateDir(filepath.Dir(r.DescriptorPath)); err != nil {
				t.Fatal(err)
			}
			if err := shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(r.DescriptorPath), "placement.json"), r); err != nil {
				t.Fatal(err)
			}
			if why == "draining" {
				f.svc.shimDraining.Store(f.req.ID, true)
			} else {
				f.svc.stops.mark(f.req.ID)
				defer f.svc.stops.clear(f.req.ID)
			}
			f.svc.shimHosting.inspect = func(context.Context, shimhost.Receipt) (shimhost.Inspection, error) {
				t.Fatal("guarded settle inspected host")
				return shimhost.Inspection{}, nil
			}
			f.svc.settleShimBridgeExit(f.req.ID)
		})
	}
}

func TestShimUnexpectedBridgeExitPreservesRunningProvider(t *testing.T) {
	f := shimFixture(t)
	r := f.start(t)
	f.svc.shimHosting.stop = func(context.Context, shimhost.Receipt) error {
		t.Error("bridge disconnect retired running provider")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Stop only the local bridge, bypassing the hosted-provider Stop path.
	if err := f.svc.Manager.Stop(ctx, f.req.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = f.svc.Manager.WaitSession(ctx, f.req.ID)
	if err := f.svc.waitShimBinding(ctx, f.req.ID); err != nil {
		t.Fatal(err)
	}
	row, _ := f.svc.Store.GetSession(f.req.ID)
	if row.State != "detached" {
		t.Fatalf("bridge disconnect state=%s", row.State)
	}
	inspect, err := f.svc.shimHosting.provider.Inspect(ctx, r)
	if err != nil || !inspect.Running {
		t.Fatalf("provider stopped on disconnect: %+v %v", inspect, err)
	}
	canonical, _ := f.svc.Store.SessionShim(ctx, f.req.ID)
	receipt, err := loadShimReceipt(canonical)
	if err != nil || receipt.Retired {
		t.Fatal("bridge disconnect retired placement")
	}
}

func TestShimSettleSignalAndGoneOutcomes(t *testing.T) {
	for _, outcome := range []string{"signal", "gone", "exit"} {
		t.Run(outcome, func(t *testing.T) {
			f := shimFixture(t)
			r := f.start(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := f.svc.DrainSessions(ctx); err != nil {
				t.Fatal(err)
			}
			retired := 0
			f.svc.shimHosting.inspect = func(context.Context, shimhost.Receipt) (shimhost.Inspection, error) {
				result := shimhost.Inspection{Receipt: r, Gone: outcome == "gone"}
				switch outcome {
				case "signal":
					result.Exit.Signal = 9
				case "exit":
					result.Exit.Status = 7
				}
				return result, nil
			}
			f.svc.shimHosting.stop = func(ctx context.Context, r shimhost.Receipt) error {
				retired++
				return f.svc.shimHosting.provider.Stop(ctx, r)
			}
			f.svc.settleShimBridgeExit(f.req.ID)
			row, _ := f.svc.Store.GetSession(f.req.ID)
			want := "failed"
			if outcome == "gone" {
				want = "orphaned"
			}
			if row.State != want || retired != 1 {
				t.Fatalf("settle outcome %s: state=%s retire=%d", outcome, row.State, retired)
			}
		})
	}
}

func TestShimReconcileExitedProviderRetiresDescriptor(t *testing.T) {
	f := shimFixture(t)
	r := f.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.svc.DrainSessions(ctx); err != nil {
		t.Fatal(err)
	}
	retired := 0
	f.svc.shimHosting.inspect = func(context.Context, shimhost.Receipt) (shimhost.Inspection, error) {
		return shimhost.Inspection{Receipt: r, Exit: shim.Exit{Status: 7}}, nil
	}
	f.svc.shimHosting.stop = func(ctx context.Context, r shimhost.Receipt) error {
		retired++
		return f.svc.shimHosting.provider.Stop(ctx, r)
	}
	f.svc.ReconcileStaleState()
	row, _ := f.svc.Store.GetSession(f.req.ID)
	if row.State != "failed" || !row.ExitCode.Valid || row.ExitCode.Int64 != 7 || retired != 1 {
		t.Fatalf("exited reconciliation lost outcome: %+v retired=%d", row, retired)
	}
	if _, err := os.Stat(r.DescriptorPath); !os.IsNotExist(err) {
		t.Fatal("exited reconciliation left descriptor")
	}
}

func TestShimLostReceiptResponseRetainsCanonicalChild(t *testing.T) {
	f := shimFixture(t)
	f.svc.shimHosting.place = func(ctx context.Context, key string, spec shim.Launch) (shimhost.Receipt, error) {
		_, err := f.svc.shimHosting.provider.Place(ctx, key, spec)
		if err != nil {
			return shimhost.Receipt{}, err
		}
		return shimhost.Receipt{}, &shimhost.Failure{Code: "outcome_unknown"}
	}
	if _, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req); err == nil || shimFailureCode(err) != "outcome_unknown" {
		t.Fatalf("lost response fell back: %v", err)
	}
	row, err := f.svc.Store.SessionShim(context.Background(), f.req.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := loadShimReceipt(row)
	if err != nil || row.HostPID == 0 || row.HostPID != receipt.HostPID {
		t.Fatalf("lost response did not recover canonical child: %+v %v", row, err)
	}
}

func TestShimUnsafeExistingReceiptForbidsFallback(t *testing.T) {
	f := shimFixture(t)
	f.svc.shimHosting.place = func(_ context.Context, key string, spec shim.Launch) (shimhost.Receipt, error) {
		r := f.svc.shimHosting.provider.PlacementIdentity(key, spec)
		path := filepath.Join(filepath.Dir(r.DescriptorPath), "placement.json")
		if err := shimhost.WritePrivateJSON(path, r); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		return shimhost.Receipt{}, &shimhost.Failure{Code: "placement_busy"}
	}
	if _, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req); err == nil || shimFailureCode(err) != "outcome_unknown" {
		t.Fatalf("unsafe receipt permitted fallback: %v", err)
	}
	row, _ := f.svc.Store.GetSession(f.req.ID)
	if row.State != "detached" {
		t.Fatalf("unsafe receipt state=%s", row.State)
	}
}

func TestShimPostPlacementCheckpointConflictRetainsChild(t *testing.T) {
	f := shimFixture(t)
	f.svc.shimHosting.place = func(ctx context.Context, key string, spec shim.Launch) (shimhost.Receipt, error) {
		r, err := f.svc.shimHosting.provider.Place(ctx, key, spec)
		if err != nil {
			return r, err
		}
		path := filepath.Join(filepath.Dir(r.DescriptorPath), "bridge.json")
		cp, err := shimbridge.ReadCheckpoint(path)
		if err != nil {
			t.Fatal(err)
		}
		cp.Journal = "wrong-journal"
		if err := shimhost.WritePrivateJSON(path, cp); err != nil {
			t.Fatal(err)
		}
		return r, nil
	}
	if _, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req); err == nil || shimFailureCode(err) != "outcome_unknown" {
		t.Fatalf("placed conflicting checkpoint started bridge: %v", err)
	}
	row, _ := f.svc.Store.GetSession(f.req.ID)
	if row.State != "detached" {
		t.Fatalf("checkpoint conflict state=%s", row.State)
	}
}

func TestShimReattachReloadsCanonicalReceipt(t *testing.T) {
	f := shimFixture(t)
	row, r := uncertainShim(t, f)
	r.Instance = "stale-instance"
	r.Journal = "stale-journal"
	if err := f.svc.reattachShim(context.Background(), row, r); err != nil {
		t.Fatalf("attach trusted stale inspect argument: %v", err)
	}
	if err := f.svc.StopSession(f.req.ID); err != nil {
		t.Fatal(err)
	}
}

func TestShimStopClosesBridgeBeforeReturning(t *testing.T) {
	f := shimFixture(t)
	f.svc.shimHosting.bridge = append(f.svc.shimHosting.bridge, "--hold-after-exit")
	f.start(t)
	result := make(chan error, 1)
	go func() { result <- f.svc.StopSession(f.req.ID) }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = f.svc.Manager.Stop(ctx, f.req.ID)
		t.Fatal("Stop waited for a bridge that requires explicit closure")
	}
	if info, live := f.svc.Manager.Get(f.req.ID); live && (info.State == agentsessions.StateRunning || info.State == agentsessions.StateLaunching) {
		t.Fatal("Stop returned with live bridge")
	}
	if _, err := currentWorkerBinding(f.svc); !errors.Is(err, registry.ErrBindingNotFound) {
		t.Fatalf("Stop returned with binding: %v", err)
	}
}

func TestShimStopWaitsForBindingSettlement(t *testing.T) {
	f := shimFixture(t)
	f.start(t)
	held := make(chan struct{})
	f.svc.shimBindingWait.Store(f.req.ID, held)
	t.Cleanup(func() {
		select {
		case <-held:
		default:
			close(held)
		}
		f.svc.shimBindingWait.CompareAndDelete(f.req.ID, held)
	})
	result := make(chan error, 1)
	go func() { result <- f.svc.StopSession(f.req.ID) }()
	// This channel is the observable settlement fence. The Stop must remain
	// blocked until it closes, regardless of whether the host is already gone.
	select {
	case err := <-result:
		t.Fatalf("Stop returned before binding settlement: %v", err)
	case <-time.After(time.Second):
	}
	close(held)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not complete after binding settlement")
	}
}

func TestShimBridgeTerminalIsNotProviderOutcome(t *testing.T) {
	f := shimFixture(t)
	f.start(t)
	exit := 7
	if err := (stateSinkAdapter{db: f.svc.Store, stops: f.svc.stops}).UpdateSessionState(f.req.ID, agentsessions.StateFailed, 0, &exit); err != nil {
		t.Fatal(err)
	}
	sink := &eventSinkAdapter{db: f.svc.Store, stops: f.svc.stops, bus: f.svc.Bus}
	sink.Emit(context.Background(), agentsessions.LifecycleEvent{SessionID: f.req.ID, From: agentsessions.StateRunning, To: agentsessions.StateFailed, ExitCode: &exit})
	row, _ := f.svc.Store.GetSession(f.req.ID)
	if row.State != "running" || row.ExitCode.Valid {
		t.Fatal("bridge exit overwrote running provider")
	}
	events, err := f.svc.Store.QueryEvents(store.EventFilter{SessionID: f.req.ID, Kinds: []string{events.KindSessionStateChanged}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		var state sessionStateChangedPayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &state); err != nil {
			t.Fatal(err)
		}
		if state.To == "failed" || state.To == "completed" || state.To == "detached" {
			t.Fatal("bridge exit published provider outcome")
		}
	}
}

func TestShimCancelledBootLaunchStillStartsBridge(t *testing.T) {
	f := shimRealLaunchFixture(t)
	f.plan.BootMode = "stdin"
	f.plan.BootPrompt = "boot-turn"
	encoded, err := json.Marshal(f.plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=? WHERE session_id=?`, string(encoded), f.req.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.svc.shimHosting.place = func(ctx context.Context, key string, spec shim.Launch) (shimhost.Receipt, error) {
		r, err := f.svc.shimHosting.provider.Place(ctx, key, spec)
		if err == nil {
			cancel()
		}
		return r, err
	}
	if _, err := f.svc.LaunchSessionWithContext(ctx, f.req.ID); err != nil {
		t.Fatalf("canceled client abandoned placed boot session: %v", err)
	}
	shimAwait(t, "boot turn from owned bridge", func() bool { return len(outputEvents(t, f.svc)) == 1 })
	if err := f.svc.SendTurn(context.Background(), f.req.ID, "next"); err != nil {
		t.Fatal(err)
	}
	shimAwait(t, "next turn after canceled boot", func() bool { return len(outputEvents(t, f.svc)) == 2 })
}

func TestShimBridgeStartFailureRetainsPlacementAndCredential(t *testing.T) {
	for _, mode := range []string{"missing", "exec-format"} {
		t.Run(mode, func(t *testing.T) {
			f := shimRealLaunchFixture(t)
			command := filepath.Join(f.root, "failed-bridge")
			if mode == "exec-format" {
				if err := os.WriteFile(command, []byte("invalid executable\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			f.svc.shimHosting.bridge = []string{command}
			if _, err := f.svc.LaunchSession(f.req.ID); err == nil || shimFailureCode(err) != "outcome_unknown" {
				t.Fatalf("bridge start failure lost typed placement outcome: %v", err)
			}
			row, _ := f.svc.Store.GetSession(f.req.ID)
			if row.State != "detached" {
				t.Fatalf("placed provider not detached after failed bridge: %s", row.State)
			}
			shimRow, err := f.svc.Store.SessionShim(context.Background(), f.req.ID)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := loadShimReceipt(shimRow)
			if err != nil {
				t.Fatal(err)
			}
			var spec shim.Launch
			if err := shimhost.ReadPrivateJSON(receipt.DescriptorPath, shim.MaxFrame, &spec); err != nil {
				t.Fatal(err)
			}
			token := ""
			for _, entry := range spec.Env {
				key, value, _ := strings.Cut(entry, "=")
				if key == "TETHER_TOKEN" {
					token = value
				}
			}
			if _, err := identity.NewStore(f.svc.Store.DB()).Verify(context.Background(), token); err != nil {
				t.Fatal("bridge failure revoked retained provider credential")
			}
			result, err := f.svc.shimHosting.provider.Inspect(context.Background(), receipt)
			if err != nil || !result.Running {
				t.Fatalf("bridge failure killed provider: %+v %v", result, err)
			}
		})
	}
}

func shimDrainWithDiagnostics(ctx context.Context, t *testing.T, f *shimAppFixture) {
	t.Helper()
	if err := f.svc.DrainSessions(ctx); err != nil {
		buf := make([]byte, 128<<10)
		n := runtime.Stack(buf, true)
		t.Logf("drain manager state: %+v", f.svc.Manager.List())
		t.Logf("drain deadline stacks:\n%s", buf[:n])
		t.Fatal(err)
	}
}
