package acpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeService is a stub Service that records calls and lets tests
// drive the SendTurn channel manually.
type fakeService struct {
	mu sync.Mutex

	launchCalls int
	launchedID  SessionID
	launchErr   error

	cancelCalls int
	closeCalls  int
	resumeCalls int

	turnCh  chan TurnUpdate
	turnErr error

	resumedFound bool
}

func newFakeService() *fakeService {
	return &fakeService{
		launchedID:   "sess_test_1",
		turnCh:       make(chan TurnUpdate, 16),
		resumedFound: true,
	}
}

func (f *fakeService) LaunchSession(_ context.Context, _ LaunchInput) (SessionID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchCalls++
	if f.launchErr != nil {
		return "", f.launchErr
	}
	return f.launchedID, nil
}

func (f *fakeService) SendTurn(_ context.Context, _ SessionID, _ []ContentBlock) (<-chan TurnUpdate, error) {
	if f.turnErr != nil {
		return nil, f.turnErr
	}
	return f.turnCh, nil
}

func (f *fakeService) CancelTurn(_ context.Context, _ SessionID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls++
	return nil
}

func (f *fakeService) CloseSession(_ context.Context, _ SessionID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return nil
}

func (f *fakeService) ResumeSession(_ context.Context, _ SessionID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumeCalls++
	if !f.resumedFound {
		return ErrSessionNotFound
	}
	return nil
}

// TestACP_HandshakeAndSessionCreate is the integration smoke per the
// sub-boot-prompt: a fake editor speaks the protocol, initializes,
// authenticates, creates a session, and reads back the session id.
func TestACP_HandshakeAndSessionCreate(t *testing.T) {
	svc := newFakeService()
	a := New(svc, "tok", []string{ScopeSessionWrite})

	// inR/inW: editor writes to inW → agent reads from inR.
	// outR/outW: agent writes to outW → editor reads from outR.
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		_ = a.Run(ctx, inR, outW)
	}()

	editor := NewWriter(inW)
	editorReader := NewReader(outR)

	// 1. initialize.
	id1, _ := json.Marshal(1)
	params1, _ := json.Marshal(InitializeParams{
		ProtocolVersion: 1,
		ClientInfo:      Implementation{Name: "test-editor", Version: "0.1"},
	})
	if err := editor.Write(&Message{ID: id1, Method: "initialize", Params: params1}); err != nil {
		t.Fatalf("editor write initialize: %v", err)
	}
	resp := mustRead(t, editorReader)
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}
	var initResult InitializeResult
	if err := json.Unmarshal(resp.Result, &initResult); err != nil {
		t.Fatalf("decode init result: %v", err)
	}
	if initResult.AgentInfo.Name != "mux" {
		t.Errorf("agent name = %q, want mux", initResult.AgentInfo.Name)
	}
	if !initResult.AgentCapabilities.SessionCapabilities.Resume {
		t.Error("expected resume capability advertised")
	}
	if len(initResult.AuthMethods) != 1 || initResult.AuthMethods[0].ID != "token" {
		t.Errorf("auth methods = %+v, want one token method", initResult.AuthMethods)
	}

	// 2. authenticate.
	id2, _ := json.Marshal(2)
	tokBody, _ := json.Marshal(AuthenticateTokenBody{Token: "tok"})
	params2, _ := json.Marshal(AuthenticateParams{MethodID: "token", Body: tokBody})
	if err := editor.Write(&Message{ID: id2, Method: "authenticate", Params: params2}); err != nil {
		t.Fatalf("editor write auth: %v", err)
	}
	resp = mustRead(t, editorReader)
	if resp.Error != nil {
		t.Fatalf("authenticate error: %+v", resp.Error)
	}

	// 3. session/new.
	id3, _ := json.Marshal(3)
	params3, _ := json.Marshal(NewSessionParams{CWD: "/tmp/work"})
	if err := editor.Write(&Message{ID: id3, Method: "session/new", Params: params3}); err != nil {
		t.Fatalf("editor write new: %v", err)
	}
	resp = mustRead(t, editorReader)
	if resp.Error != nil {
		t.Fatalf("session/new error: %+v", resp.Error)
	}
	var newResult NewSessionResult
	_ = json.Unmarshal(resp.Result, &newResult)
	if newResult.SessionID != svc.launchedID {
		t.Errorf("session id = %q, want %q", newResult.SessionID, svc.launchedID)
	}
}

// TestACP_AuthRequired verifies that scope-gated handlers reject
// before authenticate has succeeded.
func TestACP_AuthRequired(t *testing.T) {
	svc := newFakeService()
	a := New(svc, "tok", []string{ScopeSessionWrite})

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = a.Run(ctx, inR, outW) }()

	editor := NewWriter(inW)
	r := NewReader(outR)

	// Skip initialize — go straight to session/new without authenticating.
	id1, _ := json.Marshal(1)
	params, _ := json.Marshal(NewSessionParams{CWD: "/tmp"})
	if err := editor.Write(&Message{ID: id1, Method: "session/new", Params: params}); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp := mustRead(t, r)
	if resp.Error == nil {
		t.Fatal("session/new without auth should error")
	}
	if !strings.Contains(resp.Error.Message, "authenticate") {
		t.Errorf("error message should reference authenticate, got %q", resp.Error.Message)
	}
}

// TestACP_SendMessageRoutesToService drives session/prompt and verifies
// the agent_message_chunk notifications and the final stop reason.
func TestACP_SendMessageRoutesToService(t *testing.T) {
	svc := newFakeService()
	a := New(svc, "", nil) // no token = pre-authed dev mode

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = a.Run(ctx, inR, outW) }()

	editor := NewWriter(inW)
	r := NewReader(outR)

	// Drive a session/prompt; concurrently feed the fake's turnCh.
	id, _ := json.Marshal(1)
	params, _ := json.Marshal(PromptParams{
		SessionID: "s1",
		Prompt:    []ContentBlock{{Type: ContentTypeText, Text: "hi"}},
	})
	if err := editor.Write(&Message{ID: id, Method: "session/prompt", Params: params}); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	// Feed the fake's chan: two text chunks then done.
	go func() {
		svc.turnCh <- TurnUpdate{Kind: TurnUpdateKindText, Text: "hello "}
		svc.turnCh <- TurnUpdate{Kind: TurnUpdateKindText, Text: "world"}
		svc.turnCh <- TurnUpdate{Kind: TurnUpdateKindDone, StopReason: StopReasonEndTurn}
		close(svc.turnCh)
	}()

	// Drain expected messages: 2 notifications, then 1 response.
	var notifs []SessionUpdateNotification
	var promptResult PromptResult
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msg := mustRead(t, r)
		switch {
		case msg.IsNotification():
			var n SessionUpdateNotification
			_ = json.Unmarshal(msg.Params, &n)
			notifs = append(notifs, n)
		case msg.IsResponse():
			if msg.Error != nil {
				t.Fatalf("prompt error: %+v", msg.Error)
			}
			_ = json.Unmarshal(msg.Result, &promptResult)
			goto done
		}
	}
	t.Fatal("did not receive prompt response in time")
done:

	if len(notifs) != 2 {
		t.Fatalf("got %d update notifications, want 2", len(notifs))
	}
	if notifs[0].Update.SessionUpdate != SessionUpdateAgentMessageChunk {
		t.Errorf("notif[0].sessionUpdate = %q", notifs[0].Update.SessionUpdate)
	}
	if notifs[0].Update.Content == nil || notifs[0].Update.Content.Text != "hello " {
		t.Errorf("notif[0] text = %+v", notifs[0].Update.Content)
	}
	if notifs[1].Update.Content == nil || notifs[1].Update.Content.Text != "world" {
		t.Errorf("notif[1] text = %+v", notifs[1].Update.Content)
	}
	if promptResult.StopReason != StopReasonEndTurn {
		t.Errorf("stopReason = %q, want end_turn", promptResult.StopReason)
	}
}

// TestACP_CancelNotifiesService verifies the session/cancel notification
// reaches Service.CancelTurn.
func TestACP_CancelNotifiesService(t *testing.T) {
	svc := newFakeService()
	a := New(svc, "", nil)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = a.Run(ctx, inR, outW) }()

	editor := NewWriter(inW)
	params, _ := json.Marshal(CancelParams{SessionID: "s1"})
	// Notification — no id field.
	if err := editor.Write(&Message{Method: "session/cancel", Params: params}); err != nil {
		t.Fatalf("write cancel: %v", err)
	}

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		svc.mu.Lock()
		c := svc.cancelCalls
		svc.mu.Unlock()
		if c > 0 {
			cancel()
			_ = outR.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("CancelTurn not invoked")
}

// TestACP_ResumeUnknownSessionReturnsError verifies that resuming an
// unknown session id surfaces ErrSessionNotFound through the dispatcher.
func TestACP_ResumeUnknownSessionReturnsError(t *testing.T) {
	svc := newFakeService()
	svc.resumedFound = false
	a := New(svc, "", nil)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = a.Run(ctx, inR, outW) }()

	editor := NewWriter(inW)
	r := NewReader(outR)
	id, _ := json.Marshal(1)
	params, _ := json.Marshal(ResumeSessionParams{SessionID: "missing"})
	if err := editor.Write(&Message{ID: id, Method: "session/resume", Params: params}); err != nil {
		t.Fatalf("write resume: %v", err)
	}
	resp := mustRead(t, r)
	if resp.Error == nil {
		t.Fatal("expected error for unknown session resume")
	}
	if !strings.Contains(resp.Error.Message, "not found") {
		t.Errorf("error %q should mention not found", resp.Error.Message)
	}
}

// TestACP_PromptRejectsImageContent verifies the capability lock: only
// text + resource_link are accepted in prompt content blocks.
func TestACP_PromptRejectsImageContent(t *testing.T) {
	svc := newFakeService()
	a := New(svc, "", nil)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = a.Run(ctx, inR, outW) }()

	editor := NewWriter(inW)
	r := NewReader(outR)
	id, _ := json.Marshal(1)
	params, _ := json.Marshal(PromptParams{
		SessionID: "s1",
		Prompt:    []ContentBlock{{Type: ContentTypeImage, MimeType: "image/png", Data: "iVBORw0KGgo="}},
	})
	if err := editor.Write(&Message{ID: id, Method: "session/prompt", Params: params}); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	resp := mustRead(t, r)
	if resp.Error == nil {
		t.Fatal("expected error for image prompt")
	}
}

// mustRead reads one message from r or t.Fatals.
func mustRead(t *testing.T, r *Reader) *Message {
	t.Helper()
	type result struct {
		msg *Message
		err error
	}
	out := make(chan result, 1)
	go func() {
		m, err := r.Read()
		out <- result{m, err}
	}()
	select {
	case res := <-out:
		if res.err != nil {
			if errors.Is(res.err, io.EOF) {
				t.Fatal("unexpected EOF reading from agent")
			}
			t.Fatalf("read: %v", res.err)
		}
		return res.msg
	case <-time.After(2 * time.Second):
		t.Fatal("read timed out")
		return nil
	}
}
