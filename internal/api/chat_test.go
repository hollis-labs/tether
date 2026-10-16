package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
)

func chatCatalog() *fakeCatalogLoader {
	return &fakeCatalogLoader{cat: &config.Catalog{
		Agents: map[string]config.Agent{"pm": {ID: "pm"}},
		Launches: map[string]config.Launch{
			"pm-launch": {ID: "pm-launch", Agent: "pm", Chat: true, DisplayName: "Portfolio Manager", Worker: "agent-os",
				Route: &launchprofile.Route{Channel: "owner-inbox", Kinds: []string{"final", "failure"}}},
			"no-worker": {ID: "no-worker", Agent: "pm", Chat: true},
			"plain":     {ID: "plain", Agent: "pm"},
		},
	}}
}

func chatHandler(svc LaunchService) http.Handler {
	return NewHandler(Deps{Service: svc, Catalog: chatCatalog()})
}

func chatSend(h http.Handler, launch, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/chat/agents/"+launch+"/send", strings.NewReader(body)))
	return rr
}

func TestChatRosterListsOnlyFlaggedLaunches(t *testing.T) {
	svc := &fakeLaunchService{
		listRes:         []store.SessionRow{{ID: "s1", LaunchID: "pm-launch", State: "running", UpdatedAt: "2026-10-12T10:00:00Z"}},
		runtimeHealthOK: map[string]bool{"s1": true},
	}
	rr := httptest.NewRecorder()
	chatHandler(svc).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/chat/agents", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	var got ListChatAgentsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != 2 || got.Agents[0].LaunchID != "no-worker" || got.Agents[0].Worker != "" {
		t.Fatalf("roster = %+v", got.Agents)
	}
	if strings.Contains(rr.Body.String(), `"worker":""`) {
		t.Fatalf("empty worker must be omitted: %s", rr.Body)
	}
	a := got.Agents[1]
	if a.LaunchID != "pm-launch" || a.Worker != "agent-os" || a.AgentID != "pm" || a.DisplayName != "Portfolio Manager" || a.Channel != "owner-inbox" {
		t.Fatalf("agent = %+v", a)
	}
	if a.Session == nil || a.Session.ID != "s1" || !a.Session.AtTurnBoundary || a.Session.LastActivityAt != "2026-10-12T10:00:00Z" {
		t.Fatalf("session = %+v", a.Session)
	}
	if svc.listOpts.LaunchID != "pm-launch" {
		t.Fatalf("list not scoped to launch: %+v", svc.listOpts)
	}
}

func TestChatRosterNoSessionIsNull(t *testing.T) {
	rr := httptest.NewRecorder()
	chatHandler(&fakeLaunchService{}).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/chat/agents", nil))
	if !strings.Contains(rr.Body.String(), `"session":null`) {
		t.Fatalf("body %s", rr.Body)
	}
}

func TestChatSendDeliversTurnToIdleRunningSession(t *testing.T) {
	svc := &fakeLaunchService{
		listRes:         []store.SessionRow{{ID: "s1", LaunchID: "pm-launch", State: "running"}},
		runtimeHealthOK: map[string]bool{"s1": true},
	}
	h := chatHandler(svc)
	body := `{"text":"hello","idempotency_key":"k1"}`
	if rr := chatSend(h, "pm-launch", body); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"mode":"turn"`) {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	// A retry with the same key must not deliver again.
	rr := chatSend(h, "pm-launch", body)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"replayed":true`) {
		t.Fatalf("retry status %d: %s", rr.Code, rr.Body)
	}
	if len(svc.inputLog) != 1 || string(svc.inputLog[0]) != "hello" {
		t.Fatalf("delivered %q", svc.inputLog)
	}
	if rr := chatSend(h, "pm-launch", `{"text":"other","idempotency_key":"k1"}`); rr.Code != http.StatusConflict || decodeErr(t, rr).Error.Code != CodeIdempotencyConflict {
		t.Fatalf("reused key: %d %s", rr.Code, rr.Body)
	}
}

func TestChatSendMidTurnIs409AgentWorking(t *testing.T) {
	svc := &fakeLaunchService{
		listRes:            []store.SessionRow{{ID: "s1", LaunchID: "pm-launch", State: "running"}},
		runtimeHealthOK:    map[string]bool{"s1": true},
		runtimeHealthState: map[string]agentsessions.LiveState{"s1": agentsessions.LiveStateProcessing},
	}
	h := chatHandler(svc)
	rr := chatSend(h, "pm-launch", `{"text":"hello","idempotency_key":"k1"}`)
	if rr.Code != http.StatusConflict || decodeErr(t, rr).Error.Code != CodeAgentWorking {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if len(svc.inputLog) != 0 || len(svc.createInputs) != 0 {
		t.Fatalf("mid-turn send must not deliver or launch: %v %v", svc.inputLog, svc.createInputs)
	}
	// The refused key is not consumed: the retry once idle delivers.
	svc.runtimeHealthState["s1"] = agentsessions.LiveStateIdle
	if rr := chatSend(h, "pm-launch", `{"text":"hello","idempotency_key":"k1"}`); rr.Code != http.StatusOK {
		t.Fatalf("retry status %d: %s", rr.Code, rr.Body)
	}
}

func TestChatSendTurnInFlightRaceIs409(t *testing.T) {
	svc := &fakeLaunchService{
		listRes:         []store.SessionRow{{ID: "s1", LaunchID: "pm-launch", State: "running"}},
		runtimeHealthOK: map[string]bool{"s1": true},
		inputErr:        agentsessions.ErrTurnInFlight,
	}
	rr := chatSend(chatHandler(svc), "pm-launch", `{"text":"hello","idempotency_key":"k1"}`)
	if rr.Code != http.StatusConflict || decodeErr(t, rr).Error.Code != CodeAgentWorking {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
}

func TestChatSendLaunchesWhenNoRunningSession(t *testing.T) {
	for name, rows := range map[string][]store.SessionRow{
		"none":    nil,
		"ended":   {{ID: "old", LaunchID: "pm-launch", State: "completed"}},
		"foreign": {{ID: "other", LaunchID: "plain", State: "running"}},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &fakeLaunchService{listRes: rows, createRes: LaunchResult{SessionID: "new"}}
			rr := chatSend(chatHandler(svc), "pm-launch", `{"text":"start here","idempotency_key":"k9"}`)
			if rr.Code != http.StatusCreated || !strings.Contains(rr.Body.String(), `"mode":"launch"`) {
				t.Fatalf("status %d: %s", rr.Code, rr.Body)
			}
			if len(svc.createInputs) != 1 {
				t.Fatalf("creates = %+v", svc.createInputs)
			}
			in := svc.createInputs[0]
			if in.LaunchID != "pm-launch" || in.Worker != "agent-os" || in.PromptAppend != "start here" || in.IdempotencyKey != "chat:pm-launch:k9" {
				t.Fatalf("create input = %+v", in)
			}
			if len(svc.launchIDs) != 1 || svc.launchIDs[0] != "new" || len(svc.inputLog) != 0 {
				t.Fatalf("launch ids %v input %v", svc.launchIDs, svc.inputLog)
			}
		})
	}
}

func TestChatSendLaunchReplayDoesNotRelaunch(t *testing.T) {
	svc := &fakeLaunchService{createRes: LaunchResult{SessionID: "new", Replayed: true}}
	rr := chatSend(chatHandler(svc), "pm-launch", `{"text":"x","idempotency_key":"k"}`)
	if rr.Code != http.StatusOK || len(svc.launchIDs) != 0 {
		t.Fatalf("status %d launches %v", rr.Code, svc.launchIDs)
	}
}

func TestChatSendValidation(t *testing.T) {
	h := chatHandler(&fakeLaunchService{})
	for name, tc := range map[string]struct {
		launch, body string
		want         int
	}{
		"unflagged launch": {"plain", `{"text":"x","idempotency_key":"k"}`, http.StatusNotFound},
		"unknown launch":   {"nope", `{"text":"x","idempotency_key":"k"}`, http.StatusNotFound},
		"empty text":       {"pm-launch", `{"text":"  ","idempotency_key":"k"}`, http.StatusBadRequest},
		"missing key":      {"pm-launch", `{"text":"x"}`, http.StatusBadRequest},
		"unknown field":    {"pm-launch", `{"text":"x","idempotency_key":"k","extra":1}`, http.StatusBadRequest},
	} {
		if rr := chatSend(h, tc.launch, tc.body); rr.Code != tc.want {
			t.Errorf("%s: status %d: %s", name, rr.Code, rr.Body)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/chat/agents/pm-launch/send", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET send: %d", rr.Code)
	}
}

func TestChatSendNoSessionNoWorkerIs409(t *testing.T) {
	svc := &fakeLaunchService{}
	rr := chatSend(chatHandler(svc), "no-worker", `{"text":"hi","idempotency_key":"k"}`)
	e := decodeErr(t, rr).Error
	if rr.Code != http.StatusConflict || e.Code != CodeWorkerNotConfigured || !strings.Contains(e.Message, "worker:") {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
	if len(svc.createInputs) != 0 || len(svc.launchIDs) != 0 {
		t.Fatalf("nothing may be created: %+v %v", svc.createInputs, svc.launchIDs)
	}
}

func TestChatSendRunningSessionNeedsNoWorker(t *testing.T) {
	svc := &fakeLaunchService{
		listRes:         []store.SessionRow{{ID: "s1", LaunchID: "no-worker", State: "running"}},
		runtimeHealthOK: map[string]bool{"s1": true},
	}
	rr := chatSend(chatHandler(svc), "no-worker", `{"text":"hi","idempotency_key":"k"}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"mode":"turn"`) {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
}

func TestChatSendWorkerFailuresAre502NamingTheWorker(t *testing.T) {
	for name, err := range map[string]error{
		"not enrolled": fmt.Errorf("worker %q: %w", "agent-os", workerhub.ErrNotEnrolled),
		"offline":      fmt.Errorf("worker %q: %w", "agent-os", workerhub.ErrOffline),
	} {
		t.Run(name, func(t *testing.T) {
			svc := &fakeLaunchService{createErr: err}
			rr := chatSend(chatHandler(svc), "pm-launch", `{"text":"hi","idempotency_key":"k"}`)
			e := decodeErr(t, rr).Error
			if rr.Code != http.StatusBadGateway || e.Code != CodeWorkerUnavailable || !strings.Contains(e.Message, "agent-os") {
				t.Fatalf("status %d: %s", rr.Code, rr.Body)
			}
		})
	}
	svc := &fakeLaunchService{createErr: errors.New("boom")}
	if rr := chatSend(chatHandler(svc), "pm-launch", `{"text":"hi","idempotency_key":"k2"}`); rr.Code != http.StatusInternalServerError {
		t.Fatalf("unrelated error status %d", rr.Code)
	}
}

func disabledChatCatalog() *fakeCatalogLoader {
	off := false
	return &fakeCatalogLoader{cat: &config.Catalog{
		Agents: map[string]config.Agent{"pm": {ID: "pm"}, "retired": {ID: "retired", Enabled: &off}},
		Launches: map[string]config.Launch{
			"pm-launch":      {ID: "pm-launch", Agent: "pm", Chat: true, Worker: "agent-os"},
			"off-launch":     {ID: "off-launch", Agent: "pm", Chat: true, Worker: "agent-os", Enabled: &off},
			"retired-launch": {ID: "retired-launch", Agent: "retired", Chat: true, Worker: "agent-os"},
		},
	}}
}

func TestChatRosterHidesDisabledAndSendIsRefused(t *testing.T) {
	svc := &fakeLaunchService{}
	h := NewHandler(Deps{Service: svc, Catalog: disabledChatCatalog()})

	list := func(query string) ListChatAgentsResponse {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/chat/agents"+query, nil))
		var got ListChatAgentsResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("%v: %s", err, rr.Body)
		}
		return got
	}
	if got := list(""); len(got.Agents) != 1 || got.Agents[0].LaunchID != "pm-launch" || !got.Agents[0].Enabled {
		t.Fatalf("roster = %+v, want only the enabled launch", got.Agents)
	}
	all := list("?include_disabled=true")
	if len(all.Agents) != 3 {
		t.Fatalf("include_disabled roster = %+v", all.Agents)
	}
	for _, a := range all.Agents {
		if a.Enabled != (a.LaunchID == "pm-launch") {
			t.Errorf("%s enabled = %v", a.LaunchID, a.Enabled)
		}
	}

	for _, id := range []string{"off-launch", "retired-launch"} {
		rr := chatSend(h, id, `{"text":"hi","idempotency_key":"k1"}`)
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), CodeAgentDisabled) {
			t.Errorf("send to %s = %d %s, want 409 agent_disabled", id, rr.Code, rr.Body)
		}
	}
}

// A session create that the launch resolver refuses as disabled is a 409
// agent_disabled, not a 500 or a 404.
func TestSessionCreateDisabledLaunchIs409(t *testing.T) {
	svc := &fakeLaunchService{createErr: fmt.Errorf("launch %q is %w (enabled: false)", "off-launch", launch.ErrDisabled)}
	h := NewHandler(Deps{Service: svc, Catalog: disabledChatCatalog()})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"launch":"off-launch"}`)))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), CodeAgentDisabled) {
		t.Fatalf("status %d: %s", rr.Code, rr.Body)
	}
}
