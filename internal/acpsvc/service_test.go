package acpsvc_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/acpadapter"
	"github.com/hollis-labs/tether/internal/acpsvc"
	"github.com/hollis-labs/tether/internal/api"
)

// fakeDaemon implements acpsvc.DaemonClient and lets tests drive the attach
// byte stream. Push raw newline-terminated NDJSON lines onto AttachCh to
// feed the per-session scanner loop. Close AttachCh to signal clean EOF
// (the attach goroutine exits and finishes any pending turn with
// StopReasonCancelled).
type fakeDaemon struct {
	mu sync.Mutex

	launchResp api.LaunchResponse
	launchErr  error
	launchN    int

	attachCh    chan []byte
	attachStart chan struct{} // closes when AttachSession is called
	attachErr   error

	sendTurnErr  error
	sendTurnArgs []string
	stopErr      error
	stopN        int
	getErr       error
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{
		attachCh:    make(chan []byte, 8),
		attachStart: make(chan struct{}),
	}
}

func (f *fakeDaemon) Launch(_ context.Context, _ string) (api.LaunchResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchN++
	return f.launchResp, f.launchErr
}

func (f *fakeDaemon) AttachSession(ctx context.Context, _ string, w io.Writer, _ int64) error {
	close(f.attachStart)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-f.attachCh:
			if !ok {
				return f.attachErr
			}
			if _, err := w.Write(line); err != nil {
				return err
			}
		}
	}
}

func (f *fakeDaemon) SendTurn(_ context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendTurnArgs = append(f.sendTurnArgs, id+":"+text)
	return f.sendTurnErr
}

func (f *fakeDaemon) StopSession(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopN++
	return f.stopErr
}

func (f *fakeDaemon) GetSession(_ context.Context, _ string) (api.SessionDTO, error) {
	return api.SessionDTO{}, f.getErr
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// awaitAttach blocks until the attach goroutine has called AttachSession on
// the fake. Prevents races where the test writes to attachCh before the
// reader is wired up.
func awaitAttach(t *testing.T, f *fakeDaemon) {
	t.Helper()
	select {
	case <-f.attachStart:
	case <-time.After(2 * time.Second):
		t.Fatal("attach goroutine never called AttachSession")
	}
}

func TestLaunchSession_NilClient_Unsupported(t *testing.T) {
	svc := acpsvc.New(nil, "demo", discardLogger())
	_, err := svc.LaunchSession(context.Background(), acpadapter.LaunchInput{})
	if !errors.Is(err, acpadapter.ErrUnsupported) {
		t.Fatalf("nil client should return ErrUnsupported; got %v", err)
	}
}

func TestLaunchSession_EmptyLaunchID(t *testing.T) {
	svc := acpsvc.New(newFakeDaemon(), "", discardLogger())
	_, err := svc.LaunchSession(context.Background(), acpadapter.LaunchInput{})
	if err == nil {
		t.Fatal("empty launchID should error")
	}
}

func TestSendTurn_RoutesDeltaAndDone(t *testing.T) {
	f := newFakeDaemon()
	f.launchResp = api.LaunchResponse{ID: "sess-1"}
	svc := acpsvc.New(f, "demo", discardLogger())

	id, err := svc.LaunchSession(context.Background(), acpadapter.LaunchInput{})
	if err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	awaitAttach(t, f)

	ch, err := svc.SendTurn(context.Background(), id, []acpadapter.ContentBlock{
		{Type: acpadapter.ContentTypeText, Text: "hi"},
	})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}

	// Feed a single text delta and then a result-Done frame.
	f.attachCh <- []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n")
	f.attachCh <- []byte(`{"type":"result"}` + "\n")

	got := drainTurn(t, ch, 2*time.Second)
	if len(got) < 2 {
		t.Fatalf("expected delta + done updates; got %d (%+v)", len(got), got)
	}
	if got[0].Kind != acpadapter.TurnUpdateKindText || got[0].Text != "hello" {
		t.Errorf("first update: got %+v want text=hello", got[0])
	}
	last := got[len(got)-1]
	if last.Kind != acpadapter.TurnUpdateKindDone || last.StopReason != acpadapter.StopReasonEndTurn {
		t.Errorf("last update: got %+v want done/end_turn", last)
	}

	if want := "sess-1:hi"; len(f.sendTurnArgs) != 1 || f.sendTurnArgs[0] != want {
		t.Errorf("SendTurn args: got %v want [%s]", f.sendTurnArgs, want)
	}
}

func TestSendTurn_CancelBeforeDoneEmitsCancelled(t *testing.T) {
	f := newFakeDaemon()
	f.launchResp = api.LaunchResponse{ID: "sess-2"}
	svc := acpsvc.New(f, "demo", discardLogger())

	id, err := svc.LaunchSession(context.Background(), acpadapter.LaunchInput{})
	if err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	awaitAttach(t, f)

	ch, err := svc.SendTurn(context.Background(), id, []acpadapter.ContentBlock{
		{Type: acpadapter.ContentTypeText, Text: "go"},
	})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}

	if err := svc.CancelTurn(context.Background(), id); err != nil {
		t.Fatalf("CancelTurn: %v", err)
	}
	f.attachCh <- []byte(`{"type":"result"}` + "\n")

	got := drainTurn(t, ch, 2*time.Second)
	last := got[len(got)-1]
	if last.Kind != acpadapter.TurnUpdateKindDone || last.StopReason != acpadapter.StopReasonCancelled {
		t.Errorf("last update after CancelTurn: got %+v want done/canceled", last)
	}
}

func TestSendTurn_ErrorEmitsRefusal(t *testing.T) {
	f := newFakeDaemon()
	f.launchResp = api.LaunchResponse{ID: "sess-3"}
	svc := acpsvc.New(f, "demo", discardLogger())

	id, err := svc.LaunchSession(context.Background(), acpadapter.LaunchInput{})
	if err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	awaitAttach(t, f)

	ch, err := svc.SendTurn(context.Background(), id, []acpadapter.ContentBlock{
		{Type: acpadapter.ContentTypeText, Text: "x"},
	})
	if err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	f.attachCh <- []byte(`{"type":"error","error":{"message":"boom"}}` + "\n")

	got := drainTurn(t, ch, 2*time.Second)
	last := got[len(got)-1]
	if last.Kind != acpadapter.TurnUpdateKindDone || last.StopReason != acpadapter.StopReasonRefusal {
		t.Errorf("last update after error: got %+v want done/refusal", last)
	}
}

func TestSendTurn_ConcurrentTurnRejected(t *testing.T) {
	f := newFakeDaemon()
	f.launchResp = api.LaunchResponse{ID: "sess-4"}
	svc := acpsvc.New(f, "demo", discardLogger())

	id, err := svc.LaunchSession(context.Background(), acpadapter.LaunchInput{})
	if err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	awaitAttach(t, f)

	if _, err := svc.SendTurn(context.Background(), id, []acpadapter.ContentBlock{
		{Type: acpadapter.ContentTypeText, Text: "first"},
	}); err != nil {
		t.Fatalf("first SendTurn: %v", err)
	}
	if _, err := svc.SendTurn(context.Background(), id, []acpadapter.ContentBlock{
		{Type: acpadapter.ContentTypeText, Text: "second"},
	}); err == nil {
		t.Fatal("second SendTurn while turn in flight should error")
	}
}

func TestCancelTurn_UnknownSession(t *testing.T) {
	svc := acpsvc.New(newFakeDaemon(), "demo", discardLogger())
	err := svc.CancelTurn(context.Background(), acpadapter.SessionID("nope"))
	if !errors.Is(err, acpadapter.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound; got %v", err)
	}
}

func TestCloseSession_IdempotentAndStopsAttach(t *testing.T) {
	f := newFakeDaemon()
	f.launchResp = api.LaunchResponse{ID: "sess-5"}
	svc := acpsvc.New(f, "demo", discardLogger())

	id, err := svc.LaunchSession(context.Background(), acpadapter.LaunchInput{})
	if err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	awaitAttach(t, f)

	if err := svc.CloseSession(context.Background(), id); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := svc.CloseSession(context.Background(), id); err != nil {
		t.Fatalf("second close should be no-op: %v", err)
	}
	if f.stopN != 1 {
		t.Errorf("StopSession should be called exactly once; got %d", f.stopN)
	}
}

func TestResumeSession_NotFoundOnDaemon(t *testing.T) {
	f := newFakeDaemon()
	f.getErr = errors.New("no session")
	svc := acpsvc.New(f, "demo", discardLogger())

	err := svc.ResumeSession(context.Background(), acpadapter.SessionID("ghost"), "")
	if !errors.Is(err, acpadapter.ErrSessionNotFound) {
		t.Fatalf("ResumeSession on missing session: got %v want ErrSessionNotFound", err)
	}
}

func TestResumeSession_Idempotent(t *testing.T) {
	f := newFakeDaemon()
	f.launchResp = api.LaunchResponse{ID: "sess-6"}
	svc := acpsvc.New(f, "demo", discardLogger())

	id, err := svc.LaunchSession(context.Background(), acpadapter.LaunchInput{})
	if err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	awaitAttach(t, f)

	// Resume on an already-tracked id should be a no-op (no second attach).
	if err := svc.ResumeSession(context.Background(), id, ""); err != nil {
		t.Fatalf("Resume of tracked session should not error; got %v", err)
	}
}

// drainTurn collects updates from the channel until it closes or a deadline
// elapses. Includes the terminal Done update if present.
func drainTurn(t *testing.T, ch <-chan acpadapter.TurnUpdate, deadline time.Duration) []acpadapter.TurnUpdate {
	t.Helper()
	out := []acpadapter.TurnUpdate{}
	timeout := time.After(deadline)
	for {
		select {
		case u, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, u)
			if u.Kind == acpadapter.TurnUpdateKindDone {
				// drain any trailing updates already buffered
				for {
					select {
					case u2, ok := <-ch:
						if !ok {
							return out
						}
						out = append(out, u2)
					default:
						return out
					}
				}
			}
		case <-timeout:
			t.Fatalf("drainTurn timed out after %s with %d updates", deadline, len(out))
		}
	}
}
