package terminalattach

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/hollis-labs/libs/agent/sdk/chathost"
	"github.com/hollis-labs/libs/agent/sdk/session"
	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/libs/mesh/core/runtimecontract"
)

type realPTY struct {
	target     string
	input      []byte
	rows, cols uint16
}

func (p *realPTY) AttachSessionWithSnapshot(_ context.Context, id string, w io.Writer, _ int64, f func(agentsessions.AttachSnapshot) error) error {
	if id != p.target {
		return ErrNotFound
	}
	if err := f(agentsessions.AttachSnapshot{OldestOffset: 3, NextOffset: 8}); err != nil {
		return err
	}
	_, err := w.Write([]byte("hello"))
	return err
}
func (p *realPTY) SendInput(id string, b []byte) error {
	if id != p.target {
		return ErrNotFound
	}
	p.input = append([]byte(nil), b...)
	return nil
}
func (p *realPTY) ResizeSession(id string, rows, cols uint16) error {
	if id != p.target {
		return ErrNotFound
	}
	p.rows, p.cols = rows, cols
	return nil
}

func TestChatHostSharesSDKLeaseAndFencesActualPTY(t *testing.T) {
	ctx := context.Background()
	h, err := chathost.Open(t.TempDir(), func(context.Context, runtimecontract.OwnerID, chathost.Permission) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	b := Binding{OwnerID: "owner", OperativeID: "agent", SeatID: "seat", ConversationID: "conversation", AttemptID: "attempt", SessionID: "live-pty", Generation: 1, Terminal: true}
	if err = h.RegisterAgent(ctx, chathost.Agent{ID: "agent", Owner: "owner", Seat: "seat", Kind: chathost.Embedded, Supervisor: chathost.App}); err != nil {
		t.Fatal(err)
	}
	r, err := h.Create(ctx, session.Record{Binding: sdkBinding(b), Phase: session.Fenced})
	if err != nil {
		t.Fatal(err)
	}
	r.Phase = session.Active
	_, err = h.CompareAndSwap(ctx, session.Scope{OwnerID: "owner", OperativeID: "agent", ConversationID: "conversation"}, r.Version, r)
	if err != nil {
		t.Fatal(err)
	}
	pty := &realPTY{target: "live-pty"}
	live := b
	adapter := &ChatHost{Host: h, PTY: pty, ResolveTarget: func(_ context.Context, target string) (Binding, error) {
		if target != "conversation" {
			return Binding{}, ErrNotFound
		}
		return live, nil
	}}
	c := New(adapter)
	view, err := c.Open(ctx, "conversation", Viewer)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.Acquire(ctx, view.ID, false); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("viewer acquired: %v", err)
	}
	var output bytes.Buffer
	var gap bool
	err = c.Observe(ctx, view.ID, &output, 1, func(w Window, g bool) error {
		gap = g
		if w.OldestOffset != 3 || w.NextOffset != 8 {
			t.Fatalf("window: %+v", w)
		}
		return nil
	})
	if err != nil || !gap || output.String() != "hello" {
		t.Fatalf("observe: %q gap=%v err=%v", output.String(), gap, err)
	}
	first, err := c.Open(ctx, "conversation", ControllerRole)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Open(ctx, "conversation", ControllerRole)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := c.Acquire(ctx, first.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.Acquire(ctx, second.ID, false); !errors.Is(err, ErrWriterHeld) {
		t.Fatalf("second writer: %v", err)
	}
	if err = c.Input(ctx, first.ID, token, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if string(pty.input) != "one" {
		t.Fatalf("input: %q", pty.input)
	}
	if err = c.Resize(ctx, first.ID, token, 40, 100); err != nil || pty.rows != 40 || pty.cols != 100 {
		t.Fatalf("resize: %d,%d %v", pty.rows, pty.cols, err)
	}
	_, secondToken, err := c.Acquire(ctx, second.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Input(ctx, first.ID, token, []byte("stale")); !errors.Is(err, ErrWriterHeld) {
		t.Fatalf("old writer: %v", err)
	}
	live.SessionID = "replacement"
	if err = c.Input(ctx, second.ID, secondToken, []byte("stale")); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("replacement target: %v", err)
	}
	if string(pty.input) != "one" {
		t.Fatalf("stale input delivered: %q", pty.input)
	}
	if err = c.Close(ctx, view.ID); err != nil {
		t.Fatal(err)
	}
}
