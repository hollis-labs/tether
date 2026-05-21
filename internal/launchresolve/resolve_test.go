package launchresolve

import (
	"errors"
	"testing"

	"github.com/hollis-labs/go-agent-launch/agentlaunch"
)

func TestResolveRuntimeBinding(t *testing.T) {
	reg := openFixture(t)

	tests := []struct {
		name        string
		runnerID    string
		wantErrIs   error // when non-nil, ResolveRuntimeBinding must fail and errors.Is must match
		wantProv    string
		wantRuntime agentlaunch.RuntimeKind
	}{
		{
			name:        "streaming-stdio provider",
			runnerID:    "claude-stream",
			wantProv:    "claude",
			wantRuntime: agentlaunch.RuntimeStreamingStdio,
		},
		{
			name:        "explicit subprocess runtime_kind",
			runnerID:    "codex-cli",
			wantProv:    "codex",
			wantRuntime: agentlaunch.RuntimeSubprocess,
		},
		{
			name:      "unresolvable runner id is a hard error",
			runnerID:  "no-such-runner",
			wantErrIs: ErrRuntimeBindingNotFound,
		},
		{
			name:      "empty runner id is a hard error",
			runnerID:  "",
			wantErrIs: ErrRuntimeBindingNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reg.ResolveRuntimeBinding(tc.runnerID)
			if tc.wantErrIs != nil {
				if err == nil {
					t.Fatalf("expected error, got binding %+v", got)
				}
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error = %v, want errors.Is %v", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Provider != tc.wantProv {
				t.Errorf("Provider = %q, want %q", got.Provider, tc.wantProv)
			}
			if got.RuntimeKind != tc.wantRuntime {
				t.Errorf("RuntimeKind = %q, want %q", got.RuntimeKind, tc.wantRuntime)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("resolved binding fails Validate: %v", err)
			}
		})
	}
}

func TestResolveRuntimeBinding_UnmappableRuntimeIsHardError(t *testing.T) {
	// api-stub resolves to a record but its "api" runtime has no
	// agentlaunch.RuntimeKind — that must be a precise hard error, not a
	// silent subprocess fallback.
	reg := openFixture(t)
	_, err := reg.ResolveRuntimeBinding("api-stub")
	if err == nil {
		t.Fatal("expected hard error for unmappable runtime, got nil")
	}
	if errors.Is(err, ErrRuntimeBindingNotFound) {
		t.Errorf("error should be an unmappable-runtime error, not not-found: %v", err)
	}
}

func TestResolveAgent(t *testing.T) {
	reg := openFixture(t)

	t.Run("success", func(t *testing.T) {
		spec, err := reg.ResolveAgent("general")
		if err != nil {
			t.Fatalf("ResolveAgent: %v", err)
		}
		if spec.ID != "general" {
			t.Errorf("ID = %q, want general", spec.ID)
		}
		if spec.Name != "General Agent" {
			t.Errorf("Name = %q, want General Agent", spec.Name)
		}
		if spec.Labels["roles"] != "general,backend" {
			t.Errorf("Labels[roles] = %q, want general,backend", spec.Labels["roles"])
		}
	})

	t.Run("unresolvable id is hard error", func(t *testing.T) {
		_, err := reg.ResolveAgent("ghost")
		if !errors.Is(err, ErrAgentNotFound) {
			t.Errorf("error = %v, want ErrAgentNotFound", err)
		}
	})
}

func TestResolveMCP(t *testing.T) {
	reg := openFixture(t)

	t.Run("success", func(t *testing.T) {
		srv, err := reg.ResolveMCP("cerberus")
		if err != nil {
			t.Fatalf("ResolveMCP: %v", err)
		}
		if srv.ID != "cerberus" {
			t.Errorf("ID = %q, want cerberus", srv.ID)
		}
		if srv.Transport != "stdio" {
			t.Errorf("Transport = %q, want stdio", srv.Transport)
		}
		if !srv.IsEnabled() {
			t.Error("server should be enabled")
		}
	})

	t.Run("backup file is not resolvable", func(t *testing.T) {
		// clockwork lives only in a .bak file; the file-backed registrar
		// skips non-YAML, so it must not resolve.
		_, err := reg.ResolveMCP("clockwork")
		if !errors.Is(err, ErrMCPServerNotFound) {
			t.Errorf("error = %v, want ErrMCPServerNotFound", err)
		}
	})
}

func TestResolveLaunch(t *testing.T) {
	reg := openFixture(t)

	t.Run("execution-template", func(t *testing.T) {
		res, err := reg.ResolveLaunch("general-claude-stream")
		if err != nil {
			t.Fatalf("ResolveLaunch: %v", err)
		}
		if res.Kind != agentlaunch.RegistryKindExecutionTemplate {
			t.Errorf("Kind = %q, want execution-template", res.Kind)
		}
		if res.Launch == nil || res.Launch.Provider != "claude-stream" {
			t.Errorf("Launch decode = %+v, want provider claude-stream", res.Launch)
		}
		if res.Source.FilePath == "" {
			t.Error("Source.FilePath should be a handle to the catalog file")
		}
	})

	t.Run("boot-spec", func(t *testing.T) {
		res, err := reg.ResolveLaunch("general.backend.main")
		if err != nil {
			t.Fatalf("ResolveLaunch boot-spec: %v", err)
		}
		if res.Kind != agentlaunch.RegistryKindBootSpec {
			t.Errorf("Kind = %q, want boot-spec", res.Kind)
		}
		if res.BootProfileID != "general.backend.main" {
			t.Errorf("BootProfileID = %q", res.BootProfileID)
		}
	})

	t.Run("unresolvable id is hard error", func(t *testing.T) {
		_, err := reg.ResolveLaunch("nope")
		if !errors.Is(err, ErrLaunchNotFound) {
			t.Errorf("error = %v, want ErrLaunchNotFound", err)
		}
	})
}

func TestResolve_DegradedModeServesFromCache(t *testing.T) {
	reg, fault, err := openWithFault(fixtureRoot(t))
	if err != nil {
		t.Fatalf("openWithFault: %v", err)
	}

	// Healthy resolve primes the last-known-good cache for this query.
	first, err := reg.ResolveRuntimeBinding("claude-stream")
	if err != nil {
		t.Fatalf("healthy resolve: %v", err)
	}

	// Directory goes down.
	fault.tripped = true

	// Same query: the degrading registrar serves the cached snapshot, so
	// the resolve still succeeds offline (D1).
	second, err := reg.ResolveRuntimeBinding("claude-stream")
	if err != nil {
		t.Fatalf("degraded resolve should fall back to cache, got: %v", err)
	}
	if second.Provider != first.Provider || second.RuntimeKind != first.RuntimeKind {
		t.Errorf("cached resolve = %+v, want %+v", second, first)
	}
	if st := reg.Status(); !st.Degraded {
		t.Errorf("registry should report degraded after fault, got %+v", st)
	}
}

func TestResolve_DegradedModeUncachedQueryIsHardError(t *testing.T) {
	reg, fault, err := openWithFault(fixtureRoot(t))
	if err != nil {
		t.Fatalf("openWithFault: %v", err)
	}

	// Trip the fault before this query was ever cached. The degrading
	// registrar reports an honest cache miss; the helper surfaces it as a
	// precise not-found rather than a phantom success.
	fault.tripped = true
	_, err = reg.ResolveAgent("general")
	if !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("error = %v, want ErrAgentNotFound wrapping the cache miss", err)
	}
	if !errors.Is(err, agentlaunch.ErrRegistryCacheMiss) {
		t.Errorf("error chain should retain ErrRegistryCacheMiss, got %v", err)
	}
}
