//go:build !windows

package shimbridge

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestLineBoundAndPerLineCommitCarry(t *testing.T) {
	for _, pending := range []string{"123456789\n", "123456789"} {
		state := Checkpoint{Partial: []byte(pending)}
		err := drainLines(&state, 8, io.Discard, func() error { return nil }, nil)
		var fault *Failure
		if !errors.As(err, &fault) || fault.Code != "line_too_long" {
			t.Fatalf("line bound: %v", err)
		}
	}
	state := Checkpoint{Cursor: "journal:12", Partial: []byte("first\nsecond\npart")}
	var carries []string
	output := &bytes.Buffer{}
	err := drainLines(&state, 8, output, func() error { carries = append(carries, string(state.Partial)); return nil }, nil)
	if err != nil || output.String() != "first\nsecond\n" || len(carries) != 2 || carries[0] != "second\npart" || carries[1] != "part" {
		t.Fatalf("line checkpoints: %q %v %v", output.String(), carries, err)
	}
}

type stalledWriter struct{}

func (stalledWriter) Write([]byte) (int, error) { return 0, nil }
func TestShortOutputDoesNotCommit(t *testing.T) {
	state := Checkpoint{Partial: []byte("line\n")}
	committed := false
	err := drainLines(&state, 8, stalledWriter{}, func() error { committed = true; return nil }, nil)
	if !errors.Is(err, io.ErrShortWrite) || committed {
		t.Fatal("incomplete pipe write committed")
	}
}
