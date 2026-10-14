package main

import "testing"

func TestFrameTurnText(t *testing.T) {
	if got := frameTurnText("hi", ""); got != "hi" {
		t.Fatalf("no reply-to: %q", got)
	}
	if got := frameTurnText("hi", " msg://agent/local/lead "); got != "[reply-to: msg://agent/local/lead]\nhi" {
		t.Fatalf("framed: %q", got)
	}
}
