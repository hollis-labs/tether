package a2aadapter_test

// adapter_test.go — T10 (messaging vNext, CW-20260906-0041) fixture
// interoperability evidence. Every test here stands up a real a2a-go
// client (agentcard.DefaultResolver + a2aclient.NewFromCard) against a
// real Adapter served over httptest.NewServer — genuine SDK-to-SDK
// protocol conformance in-process, never a live network deployment, per
// this task's own scope text ("use fixture peers, not live federation
// deployment").

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/hollis-labs/tether/internal/a2aadapter"
	"github.com/hollis-labs/tether/internal/store"
)

const targetURN = "msg://agent/agent-mux/agt_a2atarget00"

// newFixture stands up a real *store.Store, an Adapter wrapping it per
// binding, and an httptest.Server serving the Adapter's Mux() directly
// (no /a2a/ prefix stripping needed in-process -- that's daemon wiring,
// exercised separately). Returns the store (for asserting what actually
// landed in canonical messaging) and the server URL (for client-side
// discovery).
func newFixture(t *testing.T, bindings ...a2aadapter.AgentBinding) (*store.Store, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "a2a-fixture.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)

	for i := range bindings {
		bindings[i].BaseURL = srv.URL
	}
	adapter, err := a2aadapter.NewAdapter(a2aadapter.Config{Bindings: bindings}, db.MessagingStore())
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	srv.Config.Handler = adapter.Mux()

	return db, srv.URL
}

func resolveCard(t *testing.T, baseURL, bindingID string) *a2a.AgentCard {
	t.Helper()
	cardURL := baseURL + "/agents/" + bindingID + a2asrv.WellKnownAgentCardPath
	card, err := agentcard.DefaultResolver.Resolve(context.Background(), cardURL)
	if err != nil {
		t.Fatalf("resolve agent card at %s: %v", cardURL, err)
	}
	return card
}

func newClient(t *testing.T, card *a2a.AgentCard) *a2aclient.Client {
	t.Helper()
	c, err := a2aclient.NewFromCard(context.Background(), card)
	if err != nil {
		t.Fatalf("a2aclient.NewFromCard: %v", err)
	}
	t.Cleanup(func() { _ = c.Destroy() })
	return c
}

func TestA2AAdapter_MessageOnlyFlow_RelaysIntoCanonicalMessaging(t *testing.T) {
	db, baseURL := newFixture(t, a2aadapter.AgentBinding{
		ID: "message-only", TargetURN: targetURN, DisplayName: "Message Only",
	})
	card := resolveCard(t, baseURL, "message-only")
	client := newClient(t, card)

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello from an A2A peer"))
	result, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	reply, ok := result.(*a2a.Message)
	if !ok {
		t.Fatalf("result = %T, want *a2a.Message (message-only must never yield a Task -- T10 acceptance #2)", result)
	}
	if reply.Role != a2a.MessageRoleAgent {
		t.Errorf("reply role = %q, want agent", reply.Role)
	}

	// Prove the relay actually rode the canonical, delivery-backed
	// messaging service -- not a side channel this package invented.
	env, list := findRelayedEnvelope(t, db, msg.ID)
	if list != 1 {
		t.Fatalf("found %d envelopes carrying a2a_message_id=%s, want exactly 1", list, msg.ID)
	}
	if env.Metadata["a2a_message_id"] != msg.ID {
		t.Errorf("relayed envelope metadata a2a_message_id = %q, want %q", env.Metadata["a2a_message_id"], msg.ID)
	}
	if _, hasTaskID := env.Metadata["a2a_task_id"]; hasTaskID {
		t.Errorf("message-only relay carries a2a_task_id metadata %q, want none", env.Metadata["a2a_task_id"])
	}
	// The Tether message's own ID must be Tether-native (a UUIDv7 minted
	// by the delivery core), never overwritten by the A2A message ID --
	// T10 acceptance #2's "A2A ... IDs never replace AgentID/SESSION"
	// extends to the message ID itself.
	if env.ID == msg.ID {
		t.Errorf("Tether envelope ID equals the A2A message ID %q -- IDs must stay distinct", msg.ID)
	}
	if _, ok, derr := db.DeliveryIDForMessage(context.Background(), env.ID); derr != nil || !ok {
		t.Errorf("relayed message has no delivery-core tracking: ok=%v err=%v (must ride the canonical delivery-backed Send path)", ok, derr)
	}
}

func TestA2AAdapter_DelegatedTaskFlow_WaitsForConsumerTransition(t *testing.T) {
	db, baseURL := newFixture(t, a2aadapter.AgentBinding{
		ID: "delegated", TargetURN: targetURN, TaskMode: true, TaskAwaitTimeout: 10 * time.Second,
	})
	card := resolveCard(t, baseURL, "delegated")
	client := newClient(t, card)

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("please do this delegated work"))

	type sendResult struct {
		result a2a.SendMessageResult
		err    error
	}
	resultC := make(chan sendResult, 1)
	go func() {
		result, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
		resultC <- sendResult{result, err}
	}()

	// Wait for the relay to land in canonical messaging (proving the
	// consumer's own view -- an ordinary incoming Tether message -- is
	// what it would see) before transitioning the task.
	var relayedTaskID string
	deadline := time.Now().Add(5 * time.Second)
	for relayedTaskID == "" && time.Now().Before(deadline) {
		env, n := findRelayedEnvelope(t, db, msg.ID)
		if n == 1 {
			relayedTaskID = env.Metadata["a2a_task_id"]
		}
		if relayedTaskID == "" {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if relayedTaskID == "" {
		t.Fatal("delegated task was never relayed into canonical messaging")
	}

	transitionResp := postTransition(t, baseURL, "delegated", relayedTaskID, map[string]any{
		"authorized_by": "msg://agent/agent-mux/consumer-operator",
		"state":         "completed",
		"result_text":   "done!",
	})
	if transitionResp.StatusCode != http.StatusOK {
		t.Fatalf("transition status = %d, want 200", transitionResp.StatusCode)
	}

	select {
	case sr := <-resultC:
		if sr.err != nil {
			t.Fatalf("SendMessage: %v", sr.err)
		}
		task, ok := sr.result.(*a2a.Task)
		if !ok {
			t.Fatalf("result = %T, want *a2a.Task", sr.result)
		}
		if task.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("task status = %s, want completed", task.Status.State)
		}
		if task.Status.Message == nil || task.Status.Message.Parts[0].Text() != "done!" {
			t.Errorf("task status message = %+v, want text %q", task.Status.Message, "done!")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SendMessage to return after transition")
	}
}

func TestA2AAdapter_DelegatedTask_NoTransitionWithinTimeout_YieldsInputRequired(t *testing.T) {
	_, baseURL := newFixture(t, a2aadapter.AgentBinding{
		ID: "delegated-timeout", TargetURN: targetURN, TaskMode: true, TaskAwaitTimeout: 100 * time.Millisecond,
	})
	card := resolveCard(t, baseURL, "delegated-timeout")
	client := newClient(t, card)

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("no one will ever transition this"))
	result, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	task, ok := result.(*a2a.Task)
	if !ok {
		t.Fatalf("result = %T, want *a2a.Task", result)
	}
	if task.Status.State != a2a.TaskStateInputRequired {
		t.Fatalf("task status = %s, want input_required (never hang forever, never silently fail)", task.Status.State)
	}
}

func TestA2AAdapter_DiscoveryAndCapabilities(t *testing.T) {
	_, baseURL := newFixture(t, a2aadapter.AgentBinding{
		ID: "discover", TargetURN: targetURN, DisplayName: "Discoverable", Description: "test agent",
	})
	card := resolveCard(t, baseURL, "discover")

	if card.Version != string(a2a.Version) {
		t.Errorf("card version = %q, want protocol version %q (supported version negotiation surface)", card.Version, a2a.Version)
	}
	if card.Capabilities.Streaming {
		t.Errorf("card advertises Streaming=true, want false (unadvertised capabilities must not be claimed)")
	}
	if card.Capabilities.PushNotifications {
		t.Errorf("card advertises PushNotifications=true, want false")
	}
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].ProtocolBinding != a2a.TransportProtocolJSONRPC {
		t.Fatalf("supported interfaces = %+v, want exactly one JSONRPC interface", card.SupportedInterfaces)
	}
	if card.SupportedInterfaces[0].ProtocolVersion != a2a.Version {
		t.Errorf("interface protocol version = %q, want %q", card.SupportedInterfaces[0].ProtocolVersion, a2a.Version)
	}
	if card.Name != "Discoverable" || card.Description != "test agent" {
		t.Errorf("card name/description = %q/%q, want the configured binding values", card.Name, card.Description)
	}
}

func TestA2AAdapter_UnsupportedStreaming_ExplicitError(t *testing.T) {
	_, baseURL := newFixture(t, a2aadapter.AgentBinding{
		ID: "no-streaming", TargetURN: targetURN,
	})
	card := resolveCard(t, baseURL, "no-streaming")
	client := newClient(t, card)

	// SendStreamingMessage's own client-side wrapper gracefully degrades
	// to plain SendMessage when the discovered card advertises
	// Streaming=false (a2aclient/client.go), so it can never observe a
	// server-side rejection here -- SubscribeToTask has no such
	// fallback (there is no non-streaming equivalent to "subscribe"), so
	// it always reaches the transport and the SDK's own
	// a2asrv.WithCapabilityChecks enforcement (checked before task
	// lookup, so even an unknown task id surfaces the capability error
	// first).
	var gotErr error
	for _, err := range client.SubscribeToTask(context.Background(), &a2a.SubscribeToTaskRequest{ID: a2a.TaskID("nonexistent-task")}) {
		if err != nil {
			gotErr = err
			break
		}
	}
	if gotErr == nil {
		t.Fatal("expected an explicit unsupported-operation error for streaming, got nil")
	}
	if !strings.Contains(gotErr.Error(), "not supported") {
		t.Errorf("error = %v, want an explicit not-supported error (a2a.ErrUnsupportedOperation)", gotErr)
	}
}

func TestA2AAdapter_AuthRequired_RejectsMissingOrWrongBearerToken(t *testing.T) {
	_, baseURL := newFixture(t, a2aadapter.AgentBinding{
		ID: "secured", TargetURN: targetURN, BearerToken: "correct-token",
	})
	card := resolveCard(t, baseURL, "secured")

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))

	// No token at all.
	noAuthClient := newClient(t, card)
	if _, err := noAuthClient.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg}); err == nil {
		t.Fatal("expected an error with no Authorization header")
	}

	// Wrong token.
	wrongCtx := a2aclient.AttachServiceParams(context.Background(), a2aclient.ServiceParams{"authorization": {"Bearer wrong-token"}})
	if _, err := noAuthClient.SendMessage(wrongCtx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}); err == nil {
		t.Fatal("expected an error with a wrong bearer token")
	}

	// Correct token.
	rightCtx := a2aclient.AttachServiceParams(context.Background(), a2aclient.ServiceParams{"authorization": {"Bearer correct-token"}})
	if _, err := noAuthClient.SendMessage(rightCtx, &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}); err != nil {
		t.Fatalf("SendMessage with correct bearer token: %v", err)
	}
}

func TestA2AAdapter_UnauthenticatedBindingHasNoTokenCheck(t *testing.T) {
	_, baseURL := newFixture(t, a2aadapter.AgentBinding{
		ID: "open", TargetURN: targetURN,
	})
	card := resolveCard(t, baseURL, "open")
	client := newClient(t, card)
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	if _, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg}); err != nil {
		t.Fatalf("SendMessage on a no-token binding: %v", err)
	}
}

// TestA2AAdapter_Transition_RequiresBearerTokenWhenBindingIsSecured is
// the exact scenario an independent review of this task found unguarded:
// the SAME external peer that submitted a delegated task learns its
// TaskID immediately from the Submitted event, then must NOT be able to
// self-resolve that task via the transition endpoint without presenting
// the binding's own configured bearer token -- doing so would defeat the
// entire "a consumer decides, not the submitting peer" guarantee this
// package is built around.
func TestA2AAdapter_Transition_RequiresBearerTokenWhenBindingIsSecured(t *testing.T) {
	db, baseURL := newFixture(t, a2aadapter.AgentBinding{
		ID: "secured-delegated", TargetURN: targetURN, TaskMode: true,
		BearerToken: "consumer-only-secret", TaskAwaitTimeout: 10 * time.Second,
	})
	card := resolveCard(t, baseURL, "secured-delegated")
	authCtx := a2aclient.AttachServiceParams(context.Background(), a2aclient.ServiceParams{"authorization": {"Bearer consumer-only-secret"}})
	client := newClient(t, card)

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("please do this delegated work"))
	type sendResult struct {
		result a2a.SendMessageResult
		err    error
	}
	resultC := make(chan sendResult, 1)
	go func() {
		result, err := client.SendMessage(authCtx, &a2a.SendMessageRequest{Message: msg})
		resultC <- sendResult{result, err}
	}()

	relayedTaskID := waitForRelayedTaskID(t, db, msg.ID)
	transitionBody := map[string]any{
		"authorized_by": "msg://agent/agent-mux/the-submitting-peer-itself",
		"state":         "completed",
		"result_text":   "done",
	}

	// The submitting peer attempts to self-resolve its own task with NO
	// token at all.
	noAuthResp := postTransition(t, baseURL, "secured-delegated", relayedTaskID, transitionBody)
	if noAuthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("transition with no bearer token: status = %d, want 401 -- the submitting peer must not be able to self-resolve its own task", noAuthResp.StatusCode)
	}

	// ...and with the WRONG token.
	wrongAuthResp := postTransition(t, baseURL, "secured-delegated", relayedTaskID, transitionBody, "wrong-token")
	if wrongAuthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("transition with wrong bearer token: status = %d, want 401", wrongAuthResp.StatusCode)
	}

	// The real consumer, presenting the correct token, CAN resolve it.
	rightAuthResp := postTransition(t, baseURL, "secured-delegated", relayedTaskID, transitionBody, "consumer-only-secret")
	if rightAuthResp.StatusCode != http.StatusOK {
		t.Fatalf("transition with correct bearer token: status = %d, want 200", rightAuthResp.StatusCode)
	}

	select {
	case sr := <-resultC:
		if sr.err != nil {
			t.Fatalf("SendMessage: %v", sr.err)
		}
		task, ok := sr.result.(*a2a.Task)
		if !ok || task.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("result = %+v, want a completed Task", sr.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SendMessage to return after the authorized transition")
	}
}

// TestA2AAdapter_Transition_CrossBindingTaskIsRejected is the second gap
// the same review found: a single taskCoordinator is shared across every
// binding (task IDs are process-wide unique UUIDs), so the transition
// endpoint must verify a task actually belongs to the binding named in
// the URL -- otherwise a caller who can reach binding A's (possibly
// unsecured) transition endpoint could resolve binding B's task,
// bypassing B's own authorization entirely.
func TestA2AAdapter_Transition_CrossBindingTaskIsRejected(t *testing.T) {
	db, baseURL := newFixture(t,
		a2aadapter.AgentBinding{ID: "binding-a", TargetURN: targetURN, TaskMode: true, TaskAwaitTimeout: 10 * time.Second},
		a2aadapter.AgentBinding{ID: "binding-b", TargetURN: targetURN, TaskMode: true, BearerToken: "b-secret", TaskAwaitTimeout: 10 * time.Second},
	)
	cardB := resolveCard(t, baseURL, "binding-b")
	authCtx := a2aclient.AttachServiceParams(context.Background(), a2aclient.ServiceParams{"authorization": {"Bearer b-secret"}})
	clientB := newClient(t, cardB)

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("work for binding B"))
	type sendResult struct {
		result a2a.SendMessageResult
		err    error
	}
	resultC := make(chan sendResult, 1)
	go func() {
		result, err := clientB.SendMessage(authCtx, &a2a.SendMessageRequest{Message: msg})
		resultC <- sendResult{result, err}
	}()

	taskID := waitForRelayedTaskID(t, db, msg.ID)

	// Attempt to resolve binding B's task through binding A's (unsecured)
	// URL -- must be rejected, and must NOT resolve the real task.
	crossResp := postTransition(t, baseURL, "binding-a", taskID, map[string]any{
		"authorized_by": "msg://agent/agent-mux/operator", "state": "completed",
	})
	if crossResp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-binding transition: status = %d, want 404 (task belongs to binding-b, not binding-a)", crossResp.StatusCode)
	}

	// The task must still be genuinely pending: resolving it correctly
	// through binding B now must still work (the wrong attempt above
	// didn't consume it).
	rightResp := postTransition(t, baseURL, "binding-b", taskID, map[string]any{
		"authorized_by": "msg://agent/agent-mux/operator", "state": "completed",
	}, "b-secret")
	if rightResp.StatusCode != http.StatusOK {
		t.Fatalf("transition via the correct binding: status = %d, want 200", rightResp.StatusCode)
	}

	select {
	case sr := <-resultC:
		if sr.err != nil {
			t.Fatalf("SendMessage: %v", sr.err)
		}
		task, ok := sr.result.(*a2a.Task)
		if !ok || task.Status.State != a2a.TaskStateCompleted {
			t.Fatalf("result = %+v, want a completed Task", sr.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SendMessage to return")
	}
}

// waitForRelayedTaskID polls the canonical message store for the
// envelope carrying a2a_message_id == messageID and returns its
// a2a_task_id metadata once present.
func waitForRelayedTaskID(t *testing.T, db *store.Store, messageID string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		env, n := findRelayedEnvelope(t, db, messageID)
		if n == 1 && env.Metadata["a2a_task_id"] != "" {
			return env.Metadata["a2a_task_id"]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("relayed task for message %s never appeared in canonical messaging", messageID)
	return ""
}

func TestA2AAdapter_Transition_UnknownOrAlreadyResolvedTaskIsConflict(t *testing.T) {
	_, baseURL := newFixture(t, a2aadapter.AgentBinding{ID: "transition-conflict", TargetURN: targetURN, TaskMode: true})
	resp := postTransition(t, baseURL, "transition-conflict", "no-such-task", map[string]any{
		"authorized_by": "msg://agent/agent-mux/operator", "state": "completed",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (no Execute call is waiting for this task id)", resp.StatusCode)
	}
	var out struct {
		Resolved bool `json:"resolved"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Resolved {
		t.Error("resolved = true, want false")
	}
}

// TestA2AAdapter_Transition_SecondCallOnAGenuinelyPendingTaskIsConflict
// covers the case TestA2AAdapter_Transition_UnknownOrAlreadyResolvedTaskIsConflict
// doesn't: a task that WAS actually pending and got resolved once, not
// merely an id that was never registered at all.
func TestA2AAdapter_Transition_SecondCallOnAGenuinelyPendingTaskIsConflict(t *testing.T) {
	db, baseURL := newFixture(t, a2aadapter.AgentBinding{ID: "double-transition", TargetURN: targetURN, TaskMode: true, TaskAwaitTimeout: 10 * time.Second})
	card := resolveCard(t, baseURL, "double-transition")
	client := newClient(t, card)

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("work"))
	resultC := make(chan a2a.SendMessageResult, 1)
	go func() {
		result, _ := client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg})
		resultC <- result
	}()
	taskID := waitForRelayedTaskID(t, db, msg.ID)

	first := postTransition(t, baseURL, "double-transition", taskID, map[string]any{
		"authorized_by": "msg://agent/agent-mux/operator", "state": "completed",
	})
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first transition: status = %d, want 200", first.StatusCode)
	}
	<-resultC // let Execute actually finish consuming the first resolution

	second := postTransition(t, baseURL, "double-transition", taskID, map[string]any{
		"authorized_by": "msg://agent/agent-mux/operator", "state": "failed",
	})
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second transition on an already-resolved (but genuinely was pending) task: status = %d, want 409", second.StatusCode)
	}
}

func TestA2AAdapter_Transition_RequiresAuthorizedByAndValidState(t *testing.T) {
	_, baseURL := newFixture(t, a2aadapter.AgentBinding{ID: "transition-validation", TargetURN: targetURN, TaskMode: true})

	resp := postTransition(t, baseURL, "transition-validation", "some-task", map[string]any{
		"state": "completed",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing authorized_by: status = %d, want 400", resp.StatusCode)
	}

	resp = postTransition(t, baseURL, "transition-validation", "some-task", map[string]any{
		"authorized_by": "not-a-urn", "state": "completed",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid authorized_by: status = %d, want 400", resp.StatusCode)
	}

	resp = postTransition(t, baseURL, "transition-validation", "some-task", map[string]any{
		"authorized_by": "msg://agent/agent-mux/operator", "state": "canceled",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("state=canceled (not accepted by this endpoint -- only completed/failed/input_required are): status = %d, want 400", resp.StatusCode)
	}
}

// findRelayedEnvelope scans the recipient's inbox listing for one carrying
// a2a_message_id == messageID, without consuming it (List, not Inbox --
// message.go's List is the non-destructive read).
func findRelayedEnvelope(t *testing.T, db *store.Store, messageID string) (env envelopeSummary, count int) {
	t.Helper()
	rows, err := db.DB().Query(`SELECT id, metadata FROM messages`)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var metaStr *string
		if err := rows.Scan(&id, &metaStr); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if metaStr == nil {
			continue
		}
		var meta map[string]string
		if err := json.Unmarshal([]byte(*metaStr), &meta); err != nil {
			continue
		}
		if meta["a2a_message_id"] == messageID {
			count++
			env = envelopeSummary{ID: id, Metadata: meta}
		}
	}
	return env, count
}

type envelopeSummary struct {
	ID       string
	Metadata map[string]string
}

// postTransition POSTs a transition request. An optional trailing
// bearerToken sets the Authorization header as "Bearer <token>"; a
// literal "" sends no Authorization header at all (as opposed to
// omitting the variadic arg entirely, which also sends none — both forms
// exist so call sites can be explicit about "deliberately no token" vs.
// "not testing auth here").
func postTransition(t *testing.T, baseURL, bindingID, taskID string, body map[string]any, bearerToken ...string) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal transition body: %v", err)
	}
	url := baseURL + "/agents/" + bindingID + "/tasks/" + taskID + "/transition"
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if len(bearerToken) > 0 && bearerToken[0] != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken[0])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}
