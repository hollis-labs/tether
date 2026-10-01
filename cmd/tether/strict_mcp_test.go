package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/daemon"
)

func TestStrictMCPResult(t *testing.T) {
	on := app.ClaudeStrictMCP(func(string) string { return "" })
	off := app.ClaudeStrictMCP(func(string) string { return "0" })
	health := func(strict bool, reason string) daemon.Health {
		return daemon.Health{Hardening: &daemon.HealthHardening{ClaudeStrictMCP: strict, ClaudeStrictMCPReason: reason}}
	}
	noDaemon := errors.New("daemon unreachable")

	for _, tc := range []struct {
		name       string
		h          daemon.Health
		err        error
		local      app.StrictMCPStatus
		wantStatus string
		wantIn     string
	}{
		{"daemon says on", health(true, "on: Claude agents load only the MCP servers Tether plants"), nil, off, statusOK, "load only"},
		// The daemon's answer wins over the doctor's own environment, both ways.
		{"daemon says off, doctor env on", health(false, "DISABLED by TETHER_CLAUDE_STRICT_MCP=0"), nil, on, statusWarn, "DISABLED"},
		{"daemon says on, doctor env off", health(true, "on"), nil, off, statusOK, "on"},
		{"daemon predates the field", daemon.Health{}, nil, on, statusWarn, "predates"},
		{"no daemon, env on", daemon.Health{}, noDaemon, on, statusOK, "would run Claude agents strict"},
		{"no daemon, env off", daemon.Health{}, noDaemon, off, statusWarn, "DISABLED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := strictMCPResult(tc.h, tc.err, tc.local)
			if r.Name != "claude-strict-mcp" || r.Status != tc.wantStatus || !strings.Contains(r.Message, tc.wantIn) {
				t.Fatalf("result = %+v; want status %s mentioning %q", r, tc.wantStatus, tc.wantIn)
			}
			if r.Status == statusWarn && r.Remedy == "" {
				t.Fatalf("a warning needs a remedy: %+v", r)
			}
		})
	}
}

func TestLogClaudeStrictMCP(t *testing.T) {
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, format+"|"+strings.Join(toStrings(args), ",")) }
	logClaudeStrictMCP(logf, app.ClaudeStrictMCP(func(string) string { return "" }))
	logClaudeStrictMCP(logf, app.ClaudeStrictMCP(func(string) string { return "0" }))
	if len(lines) != 2 || strings.HasPrefix(lines[0], "WARN") || !strings.HasPrefix(lines[1], "WARN: ") {
		t.Fatalf("lines = %q; want a plain line when on and a WARN when off", lines)
	}
}

func toStrings(args []any) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i], _ = a.(string)
	}
	return out
}
