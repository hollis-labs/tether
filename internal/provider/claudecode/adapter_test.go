package claudecode

import (
	"strings"
	"testing"

	"github.com/chrispian/agent-mux/internal/launch"
)

// TestAdapter_Build_DefaultMergeInheritsOsEnviron proves that with the
// default (merge) mode the child command sees the daemon's own PATH — this
// is the exact bug v0.0.1 shipped with, where plan.Env replaced the full
// environment and children lost PATH/HOME/SHELL.
func TestAdapter_Build_DefaultMergeInheritsOsEnviron(t *testing.T) {
	t.Setenv("PATH", "/sentinel/bin:/usr/bin")
	t.Setenv("MUXTEST_MARKER", "1")

	plan := &launch.Plan{Command: "/bin/echo", EnvMode: "merge"}
	cmd, err := Adapter{}.Build(plan, "/tmp")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if !containsEnv(cmd.Env, "PATH=/sentinel/bin:/usr/bin") {
		t.Fatalf("expected child PATH to inherit daemon PATH; env=%v", cmd.Env)
	}
	if !containsEnv(cmd.Env, "MUXTEST_MARKER=1") {
		t.Fatalf("expected child to inherit arbitrary parent vars; env=%v", cmd.Env)
	}
}

func TestAdapter_Build_WhitelistDropsUnlistedKeys(t *testing.T) {
	t.Setenv("PATH", "/sentinel/bin")
	t.Setenv("MUXTEST_SHOULD_DROP", "leak")

	plan := &launch.Plan{
		Command:        "/bin/echo",
		EnvMode:        "whitelist",
		EnvPassthrough: []string{"PATH"},
	}
	cmd, err := Adapter{}.Build(plan, "/tmp")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if !containsEnv(cmd.Env, "PATH=/sentinel/bin") {
		t.Fatalf("whitelist should retain PATH; env=%v", cmd.Env)
	}
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "MUXTEST_SHOULD_DROP=") {
			t.Fatalf("whitelist should drop unlisted MUXTEST_SHOULD_DROP; env=%v", cmd.Env)
		}
	}
}

func TestAdapter_Build_OverridesWin(t *testing.T) {
	t.Setenv("PATH", "/parent/bin")

	plan := &launch.Plan{
		Command: "/bin/echo",
		EnvMode: "merge",
		Env:     map[string]string{"PATH": "/override/bin", "EXTRA": "x"},
	}
	cmd, err := Adapter{}.Build(plan, "/tmp")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !containsEnv(cmd.Env, "PATH=/override/bin") {
		t.Fatalf("override should beat parent PATH; env=%v", cmd.Env)
	}
	if !containsEnv(cmd.Env, "EXTRA=x") {
		t.Fatalf("override should add new keys; env=%v", cmd.Env)
	}
}

func TestAdapter_Build_RedactDropsParentKey(t *testing.T) {
	t.Setenv("PATH", "/parent/bin")
	t.Setenv("MUXTEST_SECRET", "shh")

	plan := &launch.Plan{
		Command:   "/bin/echo",
		EnvMode:   "merge",
		EnvRedact: []string{"MUXTEST_SECRET"},
	}
	cmd, err := Adapter{}.Build(plan, "/tmp")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "MUXTEST_SECRET=") {
			t.Fatalf("redact should drop MUXTEST_SECRET; env=%v", cmd.Env)
		}
	}
}

func TestAdapter_Build_EmptyCommandErrors(t *testing.T) {
	_, err := Adapter{}.Build(&launch.Plan{}, "/tmp")
	if err == nil {
		t.Fatal("expected empty-command error")
	}
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}
