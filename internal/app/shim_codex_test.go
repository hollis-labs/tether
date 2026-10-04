//go:build !windows

package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"
	gopevents "github.com/hollis-labs/go-providers/provider/events"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
)

// Canonical isolated records model proof dispositions, not real provider/PID
// absence. No process or model CLI is started by these output-safety fixtures.
func codexOutputFixture(t *testing.T) (*Service, shimhost.Receipt, *store.CodexProtocolStore) {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := &Service{Store: db}
	if err := db.CreateSession(store.SessionRow{ID: "codex", State: "running"}, &launch.Plan{ProviderBrand: "codex"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "placement")
	if err := shimhost.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	r := shimhost.Receipt{Session: "codex", Instance: "i", Generation: 1, OperationKey: "p", DescriptorPath: filepath.Join(dir, "launch.json"), SocketPath: filepath.Join(dir, "control.sock"), Backend: shimhost.Detached, Journal: "j", Fingerprint: "fingerprint", SubmissionAttemptID: "attempt", Attempted: true, HostPID: 42, HostStartTime: 3, ProviderPID: 43}
	if err := shimhost.WritePrivateJSON(filepath.Join(dir, "placement.json"), r); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSessionShim(context.Background(), store.SessionShimRow{SessionID: r.Session, ShimKey: r.OperationKey, HostBackend: r.Backend, SocketPath: r.SocketPath, DescriptorPath: r.DescriptorPath, JournalID: r.Journal, Runtime: "codex-app", RuntimeGeneration: 1, BootGeneration: "boot", HostPID: r.HostPID, ShimPID: r.HostPID, ProviderPID: r.ProviderPID}); err != nil {
		t.Fatal(err)
	}
	p, err := db.CodexProtocolStore(context.Background(), r.Session, r.OperationKey, codexRecordLimit)
	if err != nil {
		t.Fatal(err)
	}
	state := shimcodex.State{Version: shimcodex.Version, Binding: codexBinding(r), Revision: 1, Epoch: 1, NextID: shimcodex.FirstID}
	if err := p.Commit(context.Background(), 0, state); err != nil {
		t.Fatal(err)
	}
	return s, r, p
}
func updateCodexOutputFixture(t *testing.T, p *store.CodexProtocolStore, change func(*shimcodex.State)) {
	t.Helper()
	state, err := p.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	previous := state.Revision
	change(&state)
	state.Revision++
	if err := p.Commit(context.Background(), previous, state); err != nil {
		t.Fatal(err)
	}
}

func TestHostedCodexOutputFlushRequiresTerminalAndDurableDrain(t *testing.T) {
	s, r, p := codexOutputFixture(t)
	if s.shouldFlushSessionOutput(context.Background(), r.Session) {
		t.Fatal("controller detach fabricated process_exited")
	}
	updateCodexOutputFixture(t, p, func(state *shimcodex.State) {
		state.Exit = &shim.Exit{Status: 0}
		state.Inbox = []shimcodex.Event{{Identity: "event", Cursor: "j:1", Raw: []byte(`{"method":"turn/completed"}`)}}
	})
	if s.shouldFlushSessionOutput(context.Background(), r.Session) {
		t.Fatal("terminal observation erased undelivered output")
	}
	updateCodexOutputFixture(t, p, func(state *shimcodex.State) { state.Inbox = nil })
	if s.shouldFlushSessionOutput(context.Background(), r.Session) {
		t.Fatal("empty inbox manufactured trusted delivery receipt")
	}
	// A replacement incarnation cannot borrow the prior terminal proof.
	r.Generation++
	if err := shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(r.DescriptorPath), "placement.json"), r); err != nil {
		t.Fatal(err)
	}
	if s.shouldFlushSessionOutput(context.Background(), r.Session) {
		t.Fatal("foreign generation borrowed terminal")
	}
}

func TestHostedCodexStartFailureBeforeAndAfterPossibleSubmit(t *testing.T) {
	s, r, _ := codexOutputFixture(t)
	if s.shouldFlushSessionOutput(context.Background(), r.Session) {
		t.Fatal("possible submit classified as failed child")
	}
	r.Retired = true
	r.PlacementFailure = "placement_failed"
	r.HostPID = 0
	r.ProviderPID = 0
	if err := shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(r.DescriptorPath), "placement.json"), r); err != nil {
		t.Fatal(err)
	}
	if !s.shouldFlushSessionOutput(context.Background(), r.Session) {
		t.Fatal("proven prechild refusal not flushed")
	}
}

func TestHostedCodexMissingDiscriminatorNeverUsesDirectInitializer(t *testing.T) {
	s, r, _ := codexOutputFixture(t)
	if _, err := s.Store.DB().Exec(`DELETE FROM codex_shim_protocol WHERE session_id=?`, r.Session); err != nil {
		t.Fatal(err)
	}
	_, tracked, err := s.hostedCodexSession(context.Background(), r.Session)
	if !tracked || err == nil {
		t.Fatal("missing protocol checkpoint allowed fresh direct initialization")
	}
	if s.shouldFlushSessionOutput(context.Background(), r.Session) {
		t.Fatal("missing checkpoint fabricated terminal")
	}
}

func TestOutputFlushDirectAndClaudeCompatibility(t *testing.T) {
	s, _, _ := codexOutputFixture(t)
	if !s.shouldFlushSessionOutput(context.Background(), "direct") {
		t.Fatal("direct behavior changed")
	}
	if err := s.Store.CreateSession(store.SessionRow{ID: "claude", State: "running"}, &launch.Plan{ProviderBrand: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.UpsertSessionShim(context.Background(), store.SessionShimRow{SessionID: "claude", ShimKey: "claude-key", HostBackend: shimhost.Detached, SocketPath: "/private/control.sock", DescriptorPath: "/private/launch.json", Runtime: "claude", RuntimeGeneration: 1, BootGeneration: "boot"}); err != nil {
		t.Fatal(err)
	}
	if !s.shouldFlushSessionOutput(context.Background(), "claude") {
		t.Fatal("Claude behavior changed")
	}
}

type codexWaitSession struct{ ended chan struct{} }

func (s *codexWaitSession) Wait() (int, error)                         { <-s.ended; return 0, nil }
func (*codexWaitSession) Stop(context.Context) error                   { return nil }
func (*codexWaitSession) SendInput(context.Context, []byte) error      { return nil }
func (*codexWaitSession) Resize(context.Context, uint16, uint16) error { return nil }
func (*codexWaitSession) Health() agentsessions.HealthStatus {
	return agentsessions.HealthStatus{Alive: true}
}
func (*codexWaitSession) CheckpointHints() (agentsessions.CheckpointHint, bool) { return nil, false }

type codexWaitRuntime struct{ session *codexWaitSession }

func (*codexWaitRuntime) ID() string   { return "fixture-codex" }
func (*codexWaitRuntime) Kind() string { return "cli" }
func (*codexWaitRuntime) Caps() agentsessions.Capabilities {
	return agentsessions.Capabilities{JsonRpcStdio: true}
}
func (*codexWaitRuntime) Prepare(context.Context) error { return nil }
func (r *codexWaitRuntime) Start(context.Context, agentsessions.StartOptions) (agentsessions.Session, error) {
	return r.session, nil
}

func TestHostedCodexManagerWaitDoesNotFlushBeforeReplayedCompletion(t *testing.T) {
	s, r, _ := codexOutputFixture(t)
	s.Bus = events.NewBus(events.BusOptions{Persister: s.Store})
	s.Catalog = &config.Catalog{Providers: map[string]config.Provider{"codex": {ID: "codex", Provider: "codex", RuntimeKind: config.RuntimeKindJSONRPCStdio}}}
	s.installTurnFeeds()
	t.Cleanup(s.stopOutputRetries)
	s.Manager = agentsessions.NewManager(nil)
	controller := &codexWaitSession{ended: make(chan struct{})}
	if err := s.Manager.Start(context.Background(), agentsessions.StartRequest{ID: r.Session, Runtime: &codexWaitRuntime{session: controller}}); err != nil {
		t.Fatal(err)
	}
	row, err := s.Store.GetSession(r.Session)
	if err != nil {
		t.Fatal(err)
	}
	output := s.newSessionTurnOutput(*row, &launch.Plan{ProviderBrand: "codex"})
	output.observeProvider(gopevents.Delta{Text: "before", Phase: "final"})
	s.turnOutputs.Store(r.Session, output)
	s.finalizeSessionOutput(context.Background(), r.Session, output)
	close(controller.ended)
	shimAwait(t, "local output finalizer completed", func() bool { _, present := s.turnOutputs.Load(r.Session); return !present })
	if got := outputEvents(t, s); len(got) != 0 {
		t.Fatalf("detach manufactured terminal output: %+v", got)
	}
	// Durable B2 delivery is modeled here as replay into the reducer, not claimed
	// implemented by this fixture. The prior text must remain unsettled.
	output.observeProvider(gopevents.Delta{Text: " after", Phase: "final"})
	output.observeProvider(gopevents.Done{})
	output.observeProvider(gopevents.Done{})
	got := outputEvents(t, s)
	if len(got) != 1 || got[0].Text != "before after" || got[0].StopReason == "process_exited" {
		t.Fatalf("replayed completion lost/duplicated output: %+v", got)
	}
}

func TestHostedCodexPlacementBindsOnlyPristineIntent(t *testing.T) {
	s, r, p := codexOutputFixture(t)
	updateCodexOutputFixture(t, p, func(state *shimcodex.State) {
		state.Epoch = 0
		state.Binding.Journal = ""
		state.Binding.Attempt = ""
		state.Binding.Fingerprint = ""
	})
	if err := s.initializeCodexShimCheckpoint(context.Background(), r); err != nil {
		t.Fatalf("pristine pre-placement intent did not bind: %v", err)
	}
	state, err := p.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Binding != codexBinding(r) {
		t.Fatal("canonical observed placement identity not committed")
	}
	changed := r
	changed.SubmissionAttemptID = "replacement"
	if err = s.initializeCodexShimCheckpoint(context.Background(), changed); err == nil {
		t.Fatal("bound attempt replaced")
	}
	updateCodexOutputFixture(t, p, func(state *shimcodex.State) { state.Binding.Attempt = ""; state.Partial = []byte("partial") })
	if err = s.initializeCodexShimCheckpoint(context.Background(), r); err == nil {
		t.Fatal("non-pristine intent rebound")
	}
}

func TestHostedCodexActualSettlementRetainsUntilBoundTerminalDrain(t *testing.T) {
	for _, kind := range []string{"pending", "empty_without_receipt", "unknown_input", "foreign_exit"} {
		t.Run(kind, func(t *testing.T) {
			s, r, p := codexOutputFixture(t)
			exit := shim.Exit{Status: 7}
			updateCodexOutputFixture(t, p, func(state *shimcodex.State) {
				state.Exit = &exit
				switch kind {
				case "pending":
					state.Inbox = []shimcodex.Event{{Identity: "j:exit", Cursor: "j:1", Raw: []byte(`{"status":7}`)}}
				case "unknown_input":
					state.NextID++
					state.Operations = []shimcodex.Operation{{ID: shimcodex.FirstID, Method: "initialize", Params: []byte(`{}`), Phase: shimcodex.Attempted}}
				case "foreign_exit":
					state.Exit = &shim.Exit{Status: 0}
				}
			})
			stops, cleanups := 0, 0
			s.shimHosting = &shimHosting{inspect: func(context.Context, shimhost.Receipt) (shimhost.Inspection, error) {
				return shimhost.Inspection{Receipt: r, Exit: exit}, nil
			}, stop: func(context.Context, shimhost.Receipt) error { stops++; return nil }}
			s.shimHosting.cleanup.Store(r.Session, func() { cleanups++ })
			s.settleShimBridgeExit(r.Session)
			row, err := s.Store.GetSession(r.Session)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != "detached" || stops != 0 || cleanups != 0 {
				t.Fatalf("unsettled custody erased: state=%s stop=%d cleanup=%d", row.State, stops, cleanups)
			}
			s.settleShimBridgeExit(r.Session)
			if stops != 0 || cleanups != 0 {
				t.Fatal("repeat observer manufactured terminal cleanup")
			}

			retained, err := p.Load(context.Background())
			if err != nil || retained.Exit == nil {
				t.Fatal("retained terminal evidence lost")
			}
		})
	}
}

type fixtureCodexDeliveryLoader struct {
	load func(context.Context, shimcodex.State, string) (*store.VerifiedCodexDelivery, error)
}

func (f fixtureCodexDeliveryLoader) LoadVerifiedCodexDelivery(ctx context.Context, state shimcodex.State, high string) (*store.VerifiedCodexDelivery, error) {
	return f.load(ctx, state, high)
}

func TestHostedCodexOptionalDeliveryRefusesAbsentErrorForgedAndStale(t *testing.T) {
	for _, kind := range []string{"absent", "unsupported", "error", "nil_proof", "zero_proof", "cancel", "changed_revision", "changed_custody"} {
		t.Run(kind, func(t *testing.T) {
			s, r, p := codexOutputFixture(t)
			updateCodexOutputFixture(t, p, func(state *shimcodex.State) {
				state.Cursor = "j:1"
				state.ReplayHighWater = "j:1"
				state.ExitCursor = "j:1"
				state.Exit = &shim.Exit{Status: 0}
			})
			state, err := p.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			s.shimHosting = &shimHosting{}
			if kind != "absent" {
				s.shimHosting.codexDelivery = fixtureCodexDeliveryLoader{load: func(ctx context.Context, observed shimcodex.State, high string) (*store.VerifiedCodexDelivery, error) {
					calls++
					if observed.Binding != codexBinding(r) || observed.Revision != state.Revision || high != "j:1" {
						t.Error("loader not bound to frozen source observation")
					}
					switch kind {
					case "unsupported":
						return s.Store.LoadVerifiedCodexDelivery(ctx, observed, high)
					case "error":
						return nil, errors.New("store verification failed")
					case "cancel":
						cancel()
					case "changed_revision":
						updateCodexOutputFixture(t, p, func(state *shimcodex.State) { state.Cursor = "j:2" })
					case "changed_custody":
						changed := r
						changed.Fingerprint = "replacement"
						if err := shimhost.WritePrivateJSON(filepath.Join(filepath.Dir(r.DescriptorPath), "placement.json"), changed); err != nil {
							t.Error(err)
						}
					}
					if kind == "nil_proof" {
						return nil, nil
					}
					return &store.VerifiedCodexDelivery{}, nil // exported zero value is NOT a proof
				}}
			}
			if err = s.loadCodexDelivery(ctx, state, "j:1", true); err == nil {
				t.Fatal("optional/forged/stale load manufactured delivery")
			}
			if kind == "absent" && calls != 0 || kind != "absent" && calls != 1 {
				t.Fatal("loader invocation ordering changed")
			}
		})
	}
}
