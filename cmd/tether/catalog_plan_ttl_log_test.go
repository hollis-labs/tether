package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestCatalogPlanTTLInvalidFallbackIsLoudAndDoesNotLogInput(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))
	defer slog.SetDefault(previous)
	const input = "invalid-sensitive-value"
	if catalogPlanTTL(input) != 0 {
		t.Fatal("invalid value did not default")
	}
	if !strings.Contains(out.String(), "level=WARN") || !strings.Contains(out.String(), "falling back to 15m") {
		t.Fatalf("missing warning: %s", out.String())
	}
	if strings.Contains(out.String(), input) {
		t.Fatal("logged untrusted environment input")
	}
	out.Reset()
	catalogPlanTTL("")
	if out.Len() != 0 {
		t.Fatal("unset TTL should use default quietly")
	}
}
