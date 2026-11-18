package claudestdio

import (
	"errors"
	"reflect"
	"testing"
)

func TestTapLines(t *testing.T) {
	tap := NewTap()
	_, _ = tap.Write([]byte(`{"a":1}` + "\n" + `{"half`)) // nobody subscribed: dropped
	var got []string
	cancel := tap.Subscribe(Subscriber{Line: func(l []byte) { got = append(got, string(l)) }})
	// Joined mid-line: the rest of that line is skipped.
	_, _ = tap.Write([]byte(`":2}` + "\n" + `{"b":`))
	_, _ = tap.Write([]byte("3}\r\n\n"))
	_, _ = tap.Write([]byte(`{"c":4}` + "\n"))
	cancel()
	_, _ = tap.Write([]byte(`{"d":5}` + "\n"))
	if want := []string{`{"b":3}`, `{"c":4}`}; !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}

	// Subscribed at a line boundary: nothing skipped.
	got = nil
	tap.Subscribe(Subscriber{Line: func(l []byte) { got = append(got, string(l)) }})
	_, _ = tap.Write([]byte(`{"e":6}` + "\n"))
	if want := []string{`{"e":6}`}; !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}

func TestTapInput(t *testing.T) {
	tap := NewTap()
	var notes, failed []InputNote
	tap.Subscribe(Subscriber{Input: func(n InputNote) { notes = append(notes, n) }, InputFailed: func(n InputNote) { failed = append(failed, n) }})
	var wrote int
	data := append(append(UserFrame("one"), '\n'), InterruptFrame("r1")...)
	data = append(append(data, '\n'), []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"two"},{"type":"text","text":"lines"}]}}`)...)
	if err := tap.Input(data, "send_input", func() error {
		wrote++
		if len(notes) != 2 {
			t.Errorf("notes before the write = %d, want 2", len(notes))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if wrote != 1 || notes[0].Text != "one" || notes[1].Text != "two\nlines" || notes[1].Seq != 2 || notes[0].From != "send_input" {
		t.Errorf("notes = %+v", notes)
	}
	boom := errors.New("boom")
	if err := tap.Input(UserFrame("three"), nil, func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if len(failed) != 1 || failed[0].Text != "three" || failed[0].Seq != 3 {
		t.Errorf("failed = %+v", failed)
	}
	tap.Close()
	tap.Close()
	if err := tap.Input(UserFrame("after"), nil, func() error { wrote++; return nil }); err != nil || wrote != 2 || len(notes) != 3 {
		t.Errorf("after Close: err %v, wrote %d, notes %d (want the write, no note)", err, wrote, len(notes))
	}
	select {
	case <-tap.Done():
	default:
		t.Error("Done not closed")
	}
}

func TestUserFrameText(t *testing.T) {
	for _, tc := range []struct {
		in   string
		text string
		ok   bool
	}{
		{string(UserFrame("hi")), "hi", true},
		{`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"x","content":"r"}]}}`, "", false},
		{`{"type":"user","message":{"content":[{"type":"image","source":{}}]}}`, "", true},
		{`{"type":"control_request","request":{"subtype":"interrupt"}}`, "", false},
		{`not json`, "", false},
		{``, "", false},
	} {
		text, ok := UserFrameText([]byte(tc.in))
		if text != tc.text || ok != tc.ok {
			t.Errorf("UserFrameText(%s) = %q %v, want %q %v", tc.in, text, ok, tc.text, tc.ok)
		}
	}
	if got := string(InterruptFrame("req_1")); got != `{"request":{"subtype":"interrupt"},"request_id":"req_1","type":"control_request"}` {
		t.Errorf("InterruptFrame = %s", got)
	}
}
