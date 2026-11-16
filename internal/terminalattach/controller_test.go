package terminalattach

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"sync"
	"testing"
	"time"
)

type fakeHost struct {
	mu       sync.Mutex
	binding  Binding
	attached map[string]Role
	lease    Lease
	seq      int
	now      *time.Time
	inputs   [][]byte
	resizes  [][2]uint16
}

func (h *fakeHost) Resolve(_ context.Context, target string) (Binding, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if target != h.binding.ConversationID && target != h.binding.SessionID {
		return Binding{}, ErrNotFound
	}
	return h.binding, nil
}
func (h *fakeHost) Attach(_ context.Context, b Binding, role Role) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if b != h.binding {
		return "", ErrStaleGeneration
	}
	h.seq++
	id := "view-" + strconv.Itoa(h.seq)
	h.attached[id] = role
	return id, nil
}
func (h *fakeHost) Detach(_ context.Context, b Binding, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if b != h.binding {
		return ErrStaleGeneration
	}
	delete(h.attached, id)
	if h.lease.WriterID == id {
		h.lease = Lease{}
	}
	return nil
}
func (h *fakeHost) Claim(_ context.Context, b Binding, id string, takeover bool) (Lease, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if b != h.binding {
		return Lease{}, ErrStaleGeneration
	}
	if h.attached[id] != ControllerRole {
		return Lease{}, ErrNotWriter
	}
	if h.lease.ID != "" && h.now.Before(h.lease.ExpiresAt) && !takeover {
		return Lease{}, ErrWriterHeld
	}
	h.seq++
	h.lease = Lease{ID: "lease-" + strconv.Itoa(h.seq), Binding: b, WriterID: id, ExpiresAt: h.now.Add(10 * time.Second)}
	return h.lease, nil
}
func (h *fakeHost) Renew(_ context.Context, l Lease) (Lease, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.valid(l); err != nil {
		return Lease{}, err
	}
	h.lease.ExpiresAt = h.now.Add(10 * time.Second)
	return h.lease, nil
}
func (h *fakeHost) valid(l Lease) error {
	if l.Binding != h.binding {
		return ErrStaleGeneration
	}
	if l.ID == "" || l.ID != h.lease.ID || l.WriterID != h.lease.WriterID || !h.now.Before(h.lease.ExpiresAt) {
		return ErrNotWriter
	}
	return nil
}
func (h *fakeHost) Observe(_ context.Context, b Binding, w io.Writer, _ int64, onWindow func(Window) error) error {
	h.mu.Lock()
	current := h.binding
	h.mu.Unlock()
	if b != current {
		return ErrStaleGeneration
	}
	if err := onWindow(Window{OldestOffset: 5, NextOffset: 10}); err != nil {
		return err
	}
	_, err := w.Write([]byte("output"))
	return err
}
func (h *fakeHost) Input(_ context.Context, l Lease, data []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.valid(l); err != nil {
		return err
	}
	h.inputs = append(h.inputs, bytes.Clone(data))
	return nil
}
func (h *fakeHost) Resize(_ context.Context, l Lease, rows, cols uint16) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.valid(l); err != nil {
		return err
	}
	h.resizes = append(h.resizes, [2]uint16{rows, cols})
	return nil
}

func newFixture() (*Controller, *fakeHost, *time.Time) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	h := &fakeHost{binding: Binding{OwnerID: "owner", OperativeID: "agent", ConversationID: "conv-1", AttemptID: "attempt-1", SessionID: "session-1", Generation: 7, Terminal: true}, attached: map[string]Role{}, now: &now}
	return New(h), h, &now
}

func TestObserveFirstAndReplayGap(t *testing.T) {
	c, h, _ := newFixture()
	ctx := context.Background()
	a, err := c.Open(ctx, "session-1", Viewer)
	if err != nil {
		t.Fatal(err)
	}
	if a.WriterUntil != (time.Time{}) || !a.Diagnostic {
		t.Fatalf("observer: %+v", a)
	}
	if _, _, err := c.Acquire(ctx, a.ID, false); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("viewer claim: %v", err)
	}
	if err := c.Input(ctx, a.ID, "", []byte("blocked")); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("viewer input: %v", err)
	}
	if err := c.Resize(ctx, a.ID, "", 24, 80); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("viewer resize: %v", err)
	}
	var got bytes.Buffer
	gap := false
	if err := c.Observe(ctx, a.ID, &got, 1, func(w Window, evicted bool) error {
		gap = evicted && w.OldestOffset == 5 && w.NextOffset == 10
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !gap || got.String() != "output" {
		t.Fatalf("gap=%v output=%q", gap, got.String())
	}
	if len(h.inputs) != 0 || len(h.resizes) != 0 {
		t.Fatal("observer reached PTY control")
	}
}

func TestCompetingWritersTakeoverExpiryAndRenew(t *testing.T) {
	c, h, now := newFixture()
	ctx := context.Background()
	a, _ := c.Open(ctx, "conv-1", ControllerRole)
	b, _ := c.Open(ctx, "conv-1", ControllerRole)
	_, first, err := c.Acquire(ctx, a.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Acquire(ctx, b.ID, false); !errors.Is(err, ErrWriterHeld) {
		t.Fatalf("second writer: %v", err)
	}
	if err := c.Input(ctx, a.ID, first, []byte("one")); err != nil {
		t.Fatal(err)
	}
	_, second, err := c.Acquire(ctx, b.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Input(ctx, a.ID, first, []byte("stale")); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("old token: %v", err)
	}
	if err := c.Resize(ctx, b.ID, second, 40, 120); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(11 * time.Second)
	if err := c.Input(ctx, b.ID, second, []byte("expired")); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("expired token: %v", err)
	}
	_, third, err := c.Acquire(ctx, a.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Renew(ctx, a.ID, third); err != nil {
		t.Fatal(err)
	}
	if err := c.Input(ctx, a.ID, third, []byte("three")); err != nil {
		t.Fatal(err)
	}
	if len(h.inputs) != 2 || string(h.inputs[0]) != "one" || string(h.inputs[1]) != "three" || len(h.resizes) != 1 {
		t.Fatalf("inputs=%q resizes=%v", h.inputs, h.resizes)
	}
}

func TestOldBindingAndExactSessionTarget(t *testing.T) {
	c, h, _ := newFixture()
	ctx := context.Background()
	a, _ := c.Open(ctx, "session-1", ControllerRole)
	_, lease, _ := c.Acquire(ctx, a.ID, false)
	h.mu.Lock()
	h.binding.Generation = 8
	h.binding.SessionID = "session-2"
	h.binding.AttemptID = "attempt-2"
	h.lease = Lease{}
	h.mu.Unlock()
	if err := c.Input(ctx, a.ID, lease, []byte("old")); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("old input: %v", err)
	}
	if err := c.Observe(ctx, a.ID, io.Discard, 0, func(Window, bool) error { return nil }); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("old stream: %v", err)
	}
	if _, err := c.Open(ctx, "session-1", Viewer); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old direct target: %v", err)
	}
	b, err := c.Open(ctx, "session-2", ControllerRole)
	if err != nil || b.Generation != 8 {
		t.Fatalf("new target: %+v %v", b, err)
	}
	if _, _, err := c.Acquire(ctx, b.ID, false); err != nil {
		t.Fatal(err)
	}
}

func TestFullBindingChangeInvalidatesAttachment(t *testing.T) {
	c, h, _ := newFixture()
	ctx := context.Background()
	a, _ := c.Open(ctx, "conv-1", ControllerRole)
	_, lease, _ := c.Acquire(ctx, a.ID, false)
	h.mu.Lock()
	h.binding.OwnerID = "new-owner"
	h.mu.Unlock()
	if err := c.Input(ctx, a.ID, lease, []byte("old")); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("owner change: %v", err)
	}
	if err := c.Close(ctx, a.ID); err != nil {
		t.Fatalf("stale attachment cleanup: %v", err)
	}
}

func TestAPIRuntimeHasNoSyntheticTerminal(t *testing.T) {
	c, h, _ := newFixture()
	h.binding.Terminal = false
	if _, err := c.Open(context.Background(), "conv-1", Viewer); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("API runtime: %v", err)
	}
}

func TestCloseRevokesWriter(t *testing.T) {
	c, _, _ := newFixture()
	ctx := context.Background()
	a, _ := c.Open(ctx, "conv-1", ControllerRole)
	b, _ := c.Open(ctx, "conv-1", ControllerRole)
	_, lease, _ := c.Acquire(ctx, a.ID, false)
	if err := c.Close(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Input(ctx, a.ID, lease, []byte("old")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("closed input: %v", err)
	}
	if _, _, err := c.Acquire(ctx, b.ID, false); err != nil {
		t.Fatal(err)
	}
}

type blockingHost struct {
	*fakeHost
	started chan struct{}
}

func (h *blockingHost) Observe(ctx context.Context, _ Binding, _ io.Writer, _ int64, onWindow func(Window) error) error {
	if err := onWindow(Window{}); err != nil {
		return err
	}
	close(h.started)
	<-ctx.Done()
	return ctx.Err()
}
func TestCloseEndsActiveStream(t *testing.T) {
	c, h, _ := newFixture()
	stream := &blockingHost{fakeHost: h, started: make(chan struct{})}
	c.host = stream
	a, err := c.Open(context.Background(), "conv-1", Viewer)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- c.Observe(context.Background(), a.ID, io.Discard, 0, func(Window, bool) error { return nil })
	}()
	<-stream.started
	if err := c.Close(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("closed stream: %v", err)
	}
}
