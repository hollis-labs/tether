package main

import (
	"io"
	"strings"
	"testing"
)

func TestReadTokenLineStopsAtNewlineAndLeavesStream(t *testing.T) {
	r := strings.NewReader("tth_abc\n{\"jsonrpc\":\"2.0\"}\n")
	token, err := readTokenLine(r)
	if err != nil || token != "tth_abc" {
		t.Fatalf("token %q err %v", token, err)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "{\"jsonrpc\":\"2.0\"}\n" {
		t.Fatalf("stream after credential = %q", rest)
	}
}

func TestReadTokenLineFailsClosed(t *testing.T) {
	for _, in := range []string{"", "\n", "no newline", "two words\n", strings.Repeat("x", 600) + "\n"} {
		if token, err := readTokenLine(strings.NewReader(in)); err == nil {
			t.Fatalf("%q accepted as %q", in, token)
		}
	}
}
