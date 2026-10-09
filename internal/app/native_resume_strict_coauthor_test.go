package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

type strictNativeCoauthorRuntime struct {
	recoveryFakeRuntime
	caps agentsessions.Capabilities
}

func (r strictNativeCoauthorRuntime) Caps() agentsessions.Capabilities { return r.caps }

type strictNativeCoauthorSession struct {
	*recoveryFakeSession
	mu               sync.Mutex
	methods          []string
	resumeIDs        []string
	resumeParams     []json.RawMessage
	initializeParams json.RawMessage
	response         json.RawMessage
	answerErr        error
	inputs           atomic.Int32
}

func (s *strictNativeCoauthorSession) SendInput(context.Context, []byte) error {
	s.inputs.Add(1)
	return errors.New("synthetic recovery prompt refused")
}

func (s *strictNativeCoauthorSession) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods = append(s.methods, method)
	if method == "initialize" {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		s.initializeParams = raw
		return json.RawMessage(`{}`), nil
	}
	if method == "thread/resume" {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		var request struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		s.resumeIDs = append(s.resumeIDs, request.ThreadID)
		s.resumeParams = append(s.resumeParams, raw)
		if s.answerErr != nil {
			return nil, s.answerErr
		}
		if s.response != nil {
			return s.response, nil
		}
		return json.RawMessage(`{"thread":{"id":"thread-old"}}`), nil
	}
	return nil, errors.New("synthetic new conversation or recovery turn refused")
}

func prepareStrictNativeCoauthorContext(t *testing.T, r *recoveryRuntime) agentsessions.StartOptions {
	t.Helper()
	credentialHome := prepareStrictNativeCoauthorCredentialHome(t)
	r.plan.NativeResumeOnly = true
	r.plan.NativeStateRoot = t.TempDir()
	r.plan.WorkRoot = t.TempDir()
	if err := os.Symlink(filepath.Join(credentialHome, "auth.json"), filepath.Join(r.plan.NativeStateRoot, "auth.json")); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(r.plan.NativeStateRoot, "sessions")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "thread-old.jsonl"), []byte("{\"synthetic_existing_thread\":\"thread-old\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.service.Store.UpsertSessionProviderMapping("resumed", "tether", r.plan.ProviderID, "thread-old"); err != nil {
		t.Fatal(err)
	}
	if err := r.service.Store.SetNativeStateRoot(context.Background(), "source", r.plan.NativeStateRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := r.service.Store.DB().ExecContext(context.Background(), `UPDATE launch_plans SET plan_json=json_set(plan_json,'$.work_root',?) WHERE session_id='source'`, r.plan.WorkRoot); err != nil {
		t.Fatal(err)
	}
	return agentsessions.StartOptions{
		SessionIDPreset: "thread-old", Workdir: r.plan.WorkRoot,
		Env: []string{"CODEX_HOME=" + r.plan.NativeStateRoot},
	}
}

func prepareStrictNativeCoauthorCredentialHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"synthetic_fixture":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	return home
}

func TestNativeResumeStrictCoauthorBindsExistingThreadWithoutRecoveryTurn(t *testing.T) {
	ctx := context.Background()
	retained := &strictNativeCoauthorSession{
		recoveryFakeSession: &recoveryFakeSession{done: make(chan struct{})},
	}
	t.Cleanup(func() { _ = retained.Stop(ctx) })
	starts := 0
	r := recoveryRuntimeRig(t, nil)
	opts := prepareStrictNativeCoauthorContext(t, r)
	opts.AutoFireFirstTurn = true
	opts.FirstTurnPayload = []byte("synthetic automatic recovery turn")
	opts.BootPrompt = "synthetic recovery prompt"
	opts.BootMode = "stdin"
	r.Runtime = strictNativeCoauthorRuntime{
		recoveryFakeRuntime: recoveryFakeRuntime{start: func(opts agentsessions.StartOptions) (agentsessions.Session, error) {
			starts++
			if opts.SessionIDPreset != "thread-old" || opts.AutoFireFirstTurn || opts.BootPrompt != "" || len(opts.FirstTurnPayload) != 0 || opts.BootMode != "none" {
				t.Fatal("strict start changed native identity or admitted automatic recovery input")
			}
			return retained, nil
		}},
		caps: agentsessions.Capabilities{ProviderSessionID: true, JsonRpcStdio: true},
	}
	mappingBefore, err := r.service.Store.GetSessionProviderMapping("source", "tether", r.plan.ProviderID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := r.Start(ctx, opts)
	if err != nil || sess != retained || starts != 1 {
		t.Fatal("strict recovery did not retain the existing native conversation", err)
	}
	retained.mu.Lock()
	methods := append([]string(nil), retained.methods...)
	ids := append([]string(nil), retained.resumeIDs...)
	params := append([]json.RawMessage(nil), retained.resumeParams...)
	initialize := append(json.RawMessage(nil), retained.initializeParams...)
	retained.mu.Unlock()
	if !reflect.DeepEqual(methods, []string{"initialize", "thread/resume"}) || !reflect.DeepEqual(ids, []string{"thread-old"}) || retained.inputs.Load() != 0 {
		t.Fatal("strict recovery did not bind only the exact existing thread")
	}
	var initialized struct {
		ClientInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	if json.Unmarshal(initialize, &initialized) != nil || initialized.ClientInfo.Name != "tether" || initialized.ClientInfo.Version != tetherClientVersion {
		t.Fatal("strict native bind omitted its client initialization")
	}
	var resumed struct {
		ThreadID     string `json:"threadId"`
		ExcludeTurns bool   `json:"excludeTurns"`
		CWD          string `json:"cwd"`
	}
	if len(params) != 1 || json.Unmarshal(params[0], &resumed) != nil || resumed.ThreadID != "thread-old" || !resumed.ExcludeTurns || resumed.CWD != r.plan.WorkRoot {
		t.Fatal("strict native bind omitted its exact context parameters")
	}
	mappingAfter, err := r.service.Store.GetSessionProviderMapping("source", "tether", r.plan.ProviderID)
	if err != nil || !reflect.DeepEqual(mappingAfter, mappingBefore) {
		t.Fatal("strict recovery changed retained native history", err)
	}
}

func TestNativeResumeStrictCoauthorBootProtectsSourceBeforeManualResume(t *testing.T) {
	prepareStrictNativeCoauthorCredentialHome(t)
	r := newCodexRig(t)
	sourceID := r.start()
	if err := r.svc.StopSession(sourceID); err != nil {
		t.Fatal(err)
	}
	r.ended(sourceID)
	ctx := context.Background()
	from, err := messaging.ParseURN("msg://agent/local/sender")
	if err != nil {
		t.Fatal(err)
	}
	to, err := messaging.ParseURN(registry.LogicalAgentBindingTarget("agent"))
	if err != nil {
		t.Fatal(err)
	}
	sent, err := r.svc.Store.MessagingStore().Send(ctx, messaging.Envelope{
		From: from, To: to, Kind: messaging.MsgKindNotice,
		Payload: json.RawMessage(`{"body":"synthetic pending native-only work"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	// No native turn has ever run. Unread mail makes this source eligible for
	// ordinary boot recovery, but cannot consent to replacing its context.
	r.svc.BootResumeSessions(ctx, BootResumeOptions{NativeOnlySourceIDs: []string{sourceID}})
	latest, err := r.svc.Store.LatestSessionForAgent(ctx, "agent")
	if err != nil || latest == nil || latest.ID != sourceID || r.count() != 0 {
		t.Fatal("boot recovery cold-started a protected source before manual resume", err)
	}
	if result, err := r.svc.ResumeLogicalAgentWithContext(ctx, "agent", api.ResumeOptions{NativeOnly: true, SourceSessionID: sourceID}); !errors.Is(err, ErrNativeOnlyUnavailable) || result.SessionID != "" {
		t.Fatal("manual strict recovery treated missing native history as consent to cold-start")
	}
	page, err := r.svc.Store.MessagingStore().List(ctx, to, store.ListFilter{UnreadOnly: true, Limit: 20})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != sent.ID || r.count() != 0 {
		t.Fatal("native-only refusal consumed pending work or started a provider", err)
	}
}

func TestNativeResumeStrictCoauthorRefusalsPreserveContext(t *testing.T) {
	for _, kind := range []string{"empty_preset", "mismatched_preset", "missing_context", "missing_workroot", "missing_mapping", "missing_auth_link", "foreign_auth_link", "retained_custody", "unsupported_provider", "native_lost", "fast_death", "unknown_reply", "partial_reply", "mismatched_reply"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			var presets []string
			retained := &strictNativeCoauthorSession{
				recoveryFakeSession: &recoveryFakeSession{done: make(chan struct{})},
			}
			t.Cleanup(func() { _ = retained.Stop(ctx) })
			r := recoveryRuntimeRig(t, nil)
			opts := prepareStrictNativeCoauthorContext(t, r)
			nativePath := filepath.Join(r.plan.NativeStateRoot, "sessions", "thread-old.jsonl")
			nativeBefore, err := os.ReadFile(nativePath)
			if err != nil {
				t.Fatal(err)
			}
			r.Runtime = strictNativeCoauthorRuntime{
				recoveryFakeRuntime: recoveryFakeRuntime{start: func(opts agentsessions.StartOptions) (agentsessions.Session, error) {
					presets = append(presets, opts.SessionIDPreset)
					if opts.SessionIDPreset == "" {
						return nil, errors.New("synthetic cold child forbidden")
					}
					if kind == "native_lost" {
						return nil, &agentsessions.SessionLostError{RequestedID: opts.SessionIDPreset, Err: errors.New("synthetic native context lost")}
					}
					return retained, nil
				}},
				caps: agentsessions.Capabilities{ProviderSessionID: true, JsonRpcStdio: true},
			}
			expectedStarts := 0
			switch kind {
			case "empty_preset":
				opts.SessionIDPreset = ""
			case "mismatched_preset":
				opts.SessionIDPreset = "foreign-thread"
			case "missing_context":
				r.plan.NativeStateRoot = ""
			case "missing_workroot":
				r.plan.WorkRoot = filepath.Join(t.TempDir(), "unavailable-recorded-workroot")
				opts.Workdir = r.plan.WorkRoot
				if _, err := r.service.Store.DB().ExecContext(ctx, `UPDATE launch_plans SET plan_json=json_set(plan_json,'$.work_root',?) WHERE session_id='source'`, r.plan.WorkRoot); err != nil {
					t.Fatal(err)
				}
			case "missing_mapping":
				if err := r.service.Store.ClearNativeResume(ctx, "source", "resumed", r.plan.ProviderID, "thread-old"); err != nil {
					t.Fatal(err)
				}
			case "missing_auth_link", "foreign_auth_link":
				link := filepath.Join(r.plan.NativeStateRoot, "auth.json")
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if kind == "foreign_auth_link" {
					foreign := filepath.Join(t.TempDir(), "auth.json")
					if err := os.WriteFile(foreign, []byte(`{"synthetic_foreign_fixture":true}`), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(foreign, link); err != nil {
						t.Fatal(err)
					}
				}
			case "retained_custody":
				if err := r.service.Store.UpsertSessionShim(ctx, store.SessionShimRow{
					SessionID: "source", ShimKey: "retained-custody", Runtime: "codex",
					RuntimeGeneration: 1, BootGeneration: "fixture-boot", JournalID: "retained-journal", HostBackend: "detached",
					DescriptorPath: filepath.Join(r.plan.NativeStateRoot, "launch.json"),
					SocketPath:     filepath.Join(r.plan.NativeStateRoot, "control.sock"),
					HostPID:        100, ShimPID: 100, ProviderPID: 101,
				}); err != nil {
					t.Fatal(err)
				}
			case "unsupported_provider":
				r.plan.ProviderBrand = "unsupported-provider"
			case "native_lost":
				expectedStarts = 1
			case "fast_death":
				expectedStarts = 1
				retained.once.Do(func() { close(retained.done) })
			case "unknown_reply":
				expectedStarts = 1
				retained.answerErr = errors.New("synthetic native outcome unknown")
			case "partial_reply":
				expectedStarts = 1
				retained.response = json.RawMessage(`{"thread":{}}`)
			case "mismatched_reply":
				expectedStarts = 1
				retained.response = json.RawMessage(`{"thread":{"id":"foreign-thread"}}`)
			}
			var custodyBefore *store.SessionShimRow
			if kind == "retained_custody" {
				row, err := r.service.Store.SessionShim(ctx, "source")
				if err != nil {
					t.Fatal(err)
				}
				custodyBefore = &row
			}
			mappingBefore, err := r.service.Store.GetSessionProviderMapping("source", "tether", r.plan.ProviderID)
			if err != nil {
				t.Fatal(err)
			}
			resumedBefore, err := r.service.Store.GetSessionProviderMapping("resumed", "tether", r.plan.ProviderID)
			if err != nil {
				t.Fatal(err)
			}
			planBefore, err := r.service.Store.GetLaunchPlan("resumed")
			if err != nil {
				t.Fatal(err)
			}
			sess, err := r.Start(ctx, opts)
			if !errors.Is(err, ErrNativeOnlyUnavailable) || sess != nil {
				t.Fatal("strict recovery admitted missing or unproven native context")
			}
			if len(presets) != expectedStarts {
				t.Fatal("strict recovery started an unexpected native or cold child")
			}
			for _, preset := range presets {
				if preset != "thread-old" {
					t.Fatal("strict recovery changed the requested native conversation")
				}
			}
			if retained.inputs.Load() != 0 {
				t.Fatal("strict recovery sent a recovery prompt")
			}
			retained.mu.Lock()
			methods := append([]string(nil), retained.methods...)
			retained.mu.Unlock()
			for _, method := range methods {
				if method != "initialize" && method != "thread/resume" {
					t.Fatal("strict recovery started a new conversation or recovery turn")
				}
			}
			mappingAfter, err := r.service.Store.GetSessionProviderMapping("source", "tether", r.plan.ProviderID)
			if err != nil || !reflect.DeepEqual(mappingAfter, mappingBefore) {
				t.Fatal("strict refusal cleared or changed retained native history", err)
			}
			resumedAfter, err := r.service.Store.GetSessionProviderMapping("resumed", "tether", r.plan.ProviderID)
			if err != nil || !reflect.DeepEqual(resumedAfter, resumedBefore) {
				t.Fatal("strict refusal changed the resumed native mapping", err)
			}
			planAfter, err := r.service.Store.GetLaunchPlan("resumed")
			if err != nil || !reflect.DeepEqual(planAfter, planBefore) || r.plan.ResumeProviderSessionID != "thread-old" {
				t.Fatal("strict refusal cleared the native launch preset", err)
			}
			nativeAfter, err := os.ReadFile(nativePath)
			if err != nil || string(nativeAfter) != string(nativeBefore) {
				t.Fatal("strict refusal changed retained native state bytes", err)
			}
			if custodyBefore != nil {
				row, err := r.service.Store.SessionShim(ctx, "source")
				if err != nil || !reflect.DeepEqual(row, *custodyBefore) {
					t.Fatal("strict refusal cleared or changed retained shim custody", err)
				}
			}
		})
	}
}

func TestNativeResumeStrictCoauthorApprovedCoordinationRootPreservesSource(t *testing.T) {
	for _, mode := range []string{"default", "approved", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r := recoveryRuntimeRig(t, nil)
			opts := prepareStrictNativeCoauthorContext(t, r)
			recordedRoot := filepath.Join(t.TempDir(), "unavailable-historical-workroot")
			if _, err := r.service.Store.DB().ExecContext(ctx, `UPDATE launch_plans SET plan_json=json_set(plan_json,'$.work_root',?) WHERE session_id='source'`, recordedRoot); err != nil {
				t.Fatal(err)
			}
			r.plan.WorkRoot = recordedRoot
			opts.Workdir = recordedRoot
			switch mode {
			case "approved":
				r.plan.NativeResumeWorkRoot = r.plan.NativeStateRoot
				r.plan.WorkRoot = r.plan.NativeStateRoot
				opts.Workdir = r.plan.NativeStateRoot
			case "foreign":
				r.plan.NativeResumeWorkRoot = t.TempDir()
				r.plan.WorkRoot = r.plan.NativeResumeWorkRoot
				opts.Workdir = r.plan.NativeResumeWorkRoot
			}
			retained := &strictNativeCoauthorSession{recoveryFakeSession: &recoveryFakeSession{done: make(chan struct{})}}
			t.Cleanup(func() { _ = retained.Stop(ctx) })
			starts := 0
			r.Runtime = strictNativeCoauthorRuntime{
				recoveryFakeRuntime: recoveryFakeRuntime{start: func(actual agentsessions.StartOptions) (agentsessions.Session, error) {
					starts++
					if actual.Workdir != r.plan.NativeStateRoot || actual.SessionIDPreset != "thread-old" {
						t.Fatal("approved coordination root changed native identity or runtime placement")
					}
					return retained, nil
				}},
				caps: agentsessions.Capabilities{ProviderSessionID: true, JsonRpcStdio: true},
			}
			sourceBefore, err := r.service.Store.GetLaunchPlan("source")
			if err != nil {
				t.Fatal(err)
			}
			mappingBefore, err := r.service.Store.GetSessionProviderMapping("source", "tether", r.plan.ProviderID)
			if err != nil {
				t.Fatal(err)
			}
			nativePath := filepath.Join(r.plan.NativeStateRoot, "sessions", "thread-old.jsonl")
			nativeBefore, err := os.ReadFile(nativePath)
			if err != nil {
				t.Fatal(err)
			}
			sess, err := r.Start(ctx, opts)
			if mode == "approved" {
				if err != nil || sess != retained || starts != 1 {
					t.Fatal("exact explicit coordination root did not preserve native recovery", err)
				}
				retained.mu.Lock()
				methods := append([]string(nil), retained.methods...)
				ids := append([]string(nil), retained.resumeIDs...)
				retained.mu.Unlock()
				if !reflect.DeepEqual(methods, []string{"initialize", "thread/resume"}) || !reflect.DeepEqual(ids, []string{"thread-old"}) || retained.inputs.Load() != 0 {
					t.Fatal("coordination-only recovery started a conversation or automatic turn")
				}
			} else if sess != nil || !errors.Is(err, ErrNativeOnlyUnavailable) || starts != 0 {
				t.Fatal("missing or foreign coordination approval admitted native execution")
			}
			sourceAfter, err := r.service.Store.GetLaunchPlan("source")
			if err != nil || !reflect.DeepEqual(sourceAfter, sourceBefore) {
				t.Fatal("coordination override rewrote historical source roots", err)
			}
			mappingAfter, err := r.service.Store.GetSessionProviderMapping("source", "tether", r.plan.ProviderID)
			if err != nil || !reflect.DeepEqual(mappingAfter, mappingBefore) {
				t.Fatal("coordination override cleared or changed retained native mapping", err)
			}
			nativeAfter, err := os.ReadFile(nativePath)
			if err != nil || string(nativeAfter) != string(nativeBefore) {
				t.Fatal("coordination override changed retained native state", err)
			}
			if _, err := os.Lstat(recordedRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("coordination override recreated the unavailable historical workroot")
			}
		})
	}
}

func TestNativeResumeStrictCoauthorBootWorkRootRequiresSelectedSource(t *testing.T) {
	for _, mode := range []string{"unselected", "foreign_key"} {
		t.Run(mode, func(t *testing.T) {
			prepareStrictNativeCoauthorCredentialHome(t)
			r := newCodexRig(t)
			sourceID := r.start()
			if err := r.svc.StopSession(sourceID); err != nil {
				t.Fatal(err)
			}
			r.ended(sourceID)
			ctx := context.Background()
			from, err := messaging.ParseURN("msg://agent/local/sender")
			if err != nil {
				t.Fatal(err)
			}
			to, err := messaging.ParseURN(registry.LogicalAgentBindingTarget("agent"))
			if err != nil {
				t.Fatal(err)
			}
			sent, err := r.svc.Store.MessagingStore().Send(ctx, messaging.Envelope{From: from, To: to, Kind: messaging.MsgKindNotice, Payload: json.RawMessage(`{"body":"synthetic pending coordination"}`)})
			if err != nil {
				t.Fatal(err)
			}
			options := BootResumeOptions{NativeOnlyWorkRoots: map[string]string{sourceID: t.TempDir()}}
			if mode == "foreign_key" {
				options.NativeOnlySourceIDs = []string{sourceID}
				options.NativeOnlyWorkRoots = map[string]string{"another-source": t.TempDir()}
			}
			r.svc.BootResumeSessions(ctx, options)
			latest, err := r.svc.Store.LatestSessionForAgent(ctx, "agent")
			if err != nil || latest == nil || latest.ID != sourceID || r.count() != 0 {
				t.Fatal("unbound workroot fell through to ordinary cold boot recovery", err)
			}
			page, err := r.svc.Store.MessagingStore().List(ctx, to, store.ListFilter{UnreadOnly: true, Limit: 20})
			if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != sent.ID {
				t.Fatal("unbound workroot selection consumed pending mail", err)
			}
		})
	}
}
