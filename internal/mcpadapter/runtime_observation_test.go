package mcpadapter

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestLaunchObservationSelectors(t *testing.T) {
	dir := t.TempDir()
	first, second, selector := filepath.Join(dir, "first"), filepath.Join(dir, "second"), filepath.Join(dir, "selector")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexec some-other-program\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(first, selector); err != nil {
		t.Fatal(err)
	}
	entry := config.MCPServerEntry{Command: selector}
	before := observeLaunch(exec.Command(entry.Command), entry)
	if before.Selector != selector || before.SelectorKind != "absolute" || before.TargetRelation != "unknown" || filepath.Base(before.ResolvedPath) != "first" {
		t.Fatalf("wrapper/symlink conflated with leaf: %+v", before)
	}
	if err := os.Remove(selector); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, selector); err != nil {
		t.Fatal(err)
	}
	after := observeLaunch(exec.Command(entry.Command), entry)
	if after.Selector != before.Selector || after.ResolvedPath == before.ResolvedPath || filepath.Base(before.ResolvedPath) != "first" {
		t.Fatalf("selector/observation were not retained separately: before=%+v after=%+v", before, after)
	}
	t.Setenv("PATH", dir)
	entry.Command = "selector"
	entry.Env = map[string]string{"PATH": "/child-only-path"}
	fromPATH := observeLaunch(exec.Command(entry.Command), entry)
	if fromPATH.SelectorKind != "path" || fromPATH.RelaunchLookup != "proxy-PATH" || filepath.Base(fromPATH.ResolvedPath) != "second" {
		t.Fatalf("did not retain actual proxy PATH lookup: %+v", fromPATH)
	}
	entry.Command = "missing-command"
	missing := observeLaunch(exec.Command(entry.Command), entry)
	if missing.Resolution != "unknown" || missing.ResolvedPath != "" {
		t.Fatalf("missing selector looks resolved: %+v", missing)
	}
	relative, err := filepath.Rel(mustWorkingDir(t), second)
	if err != nil {
		t.Fatal(err)
	}
	entry.Command = relative
	rel := observeLaunch(exec.Command(entry.Command), entry)
	if rel.Selector != relative || rel.SelectorKind != "relative" || rel.RelaunchLookup != "proxy-working-directory" {
		t.Fatalf("relative selector lost: %+v", rel)
	}
	entry.Command, entry.Token = first, "first"
	redacted := observeLaunch(exec.Command(entry.Command), entry)
	if redacted.Selector != "" || redacted.ResolvedPath != "" || redacted.Resolution != "redacted" {
		t.Fatalf("credential-bearing path escaped: %+v", redacted)
	}
}

func mustWorkingDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRuntimeObservationWireAndRecovery(t *testing.T) {
	dir := t.TempDir()
	entry := fixtureEntry(t, dir, "alpha")
	entry.Env["TETHER_UPSTREAM_RECORD_INITIALIZE"] = "1"
	entry.Args = append(entry.Args, "-test.timeout=30s")
	pool := NewClientPool([]config.MCPServerEntry{entry}, NewToolRegistry())
	pool.runtime.Build.Version = "review-build"
	pool.runtime.Build.Commit = "embedded-before-replacement"
	pool.policy.delays = []time.Duration{10 * time.Millisecond}
	defer pool.Shutdown()
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := awaitStatus(t, pool, "alpha", func(s ServerStatus) bool { return s.Status == "connected" })
	read := func() RelaunchObservation {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, "alpha.initialize.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"secret-fixture-token", "-test.timeout", "TETHER_UPSTREAM_NAME", "TETHER_UPSTREAM_FIXTURE"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("private launch data in initialization: %s", secret)
			}
		}
		var req mcp.InitializeRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatal(err)
		}
		if req.Params.ClientInfo.Version != "review-build" {
			t.Fatalf("stale client version: %+v", req.Params.ClientInfo)
		}
		data, err := json.Marshal(req.Params.Capabilities.Experimental[RuntimeObservationCapability])
		if err != nil {
			t.Fatal(err)
		}
		var observation RelaunchObservation
		if err := json.Unmarshal(data, &observation); err != nil {
			t.Fatal(err)
		}
		return observation
	}
	before := read()
	if before.Owner.InstanceID == "" || before.Owner.PID != os.Getpid() || before.Owner.Build.Commit != "embedded-before-replacement" || before.Launch.PID != first.LastLaunch.PID || before.Recovery.AttemptsRemaining != 1 || before.Recovery.ExitPermitted || before.Mode != "observation-only" {
		t.Fatalf("incorrect supervising owner/context: %+v", before)
	}
	fixtureMarker(t, dir, "alpha", "0")
	second := awaitStatus(t, pool, "alpha", func(s ServerStatus) bool { return s.Status == "connected" && s.RestartAttempts == 1 })
	after := read()
	if after.Owner.InstanceID != before.Owner.InstanceID || after.Launch.PID == before.Launch.PID || after.Launch.PID != second.LastLaunch.PID || after.Recovery.AttemptsRemaining != 0 || after.Recovery.ExitPermitted || after.Recovery.Reservation != "none" {
		t.Fatalf("retry admission or owner misrepresented: %+v", after)
	}
	fixtureMarker(t, dir, "alpha", "0")
	exhausted := awaitStatus(t, pool, "alpha", func(s ServerStatus) bool { return s.RecoveryExhausted })
	if exhausted.Recovery.AttemptsRemaining != 0 || exhausted.Recovery.ExitPermitted {
		t.Fatalf("exhausted recovery authorized exit: %+v", exhausted)
	}
}

func TestRuntimeObservationHealthUsesEmbeddedMetadata(t *testing.T) {
	a := newTestAdapter(t)
	a.svc.Catalog = &config.Catalog{}
	a.SetBuildMetadata("candidate-A", "source-A", "build-A")
	result, err := a.handleHealth(context.Background(), mcp.CallToolRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Version string             `json:"version"`
		Runtime RuntimeObservation `json:"runtime"`
	}
	if err := json.Unmarshal([]byte(textOf(result)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Version != "candidate-A" || payload.Runtime.Build.Commit != "source-A" || payload.Runtime.InstanceID != processObservation.InstanceID || payload.Runtime.Build.ImageIdentity != "unknown" {
		t.Fatalf("running metadata missing or labels promoted to image proof: %+v", payload)
	}
	// Remote transports and synthetic clients do not supervise a local child.
	pool := NewClientPool(nil, NewToolRegistry())
	if req := pool.initializeRequest(config.MCPServerEntry{Transport: "http"}, &mockClient{}); len(req.Params.Capabilities.Experimental) != 0 {
		t.Fatal("advertised local recovery for a remote transport")
	}
}
