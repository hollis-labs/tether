package specresolve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/agentkit/agentlaunch"

	"github.com/hollis-labs/tether/internal/launchresolve"
)

// corpusRoot is the in-repo S5 LaunchSpec corpus. It lives at the repo
// root (testdata/launch-specs) and is shared with the parity harness, so
// the resolver tests reference it rather than duplicating it.
func corpusRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "testdata", "launch-specs"))
	if err != nil {
		t.Fatalf("resolve corpus root: %v", err)
	}
	return abs
}

// catalogRoot is the package-local fixture catalog: runtime-binding
// (providers/) and agent-source (agents/) records covering every runner
// and agent the launch corpus references. It is independent of the live
// ~/.tether/catalog/.
func catalogRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "catalog"))
	if err != nil {
		t.Fatalf("resolve catalog root: %v", err)
	}
	return abs
}

// openRegistry builds a Registry over the fixture catalog.
func openRegistry(t *testing.T) *launchresolve.Registry {
	t.Helper()
	reg, err := launchresolve.OpenAt(launchresolve.Options{CatalogRoot: catalogRoot(t)})
	if err != nil {
		t.Fatalf("open fixture registry: %v", err)
	}
	return reg
}

// newResolver builds a Resolver over the in-repo corpus and fixture
// catalog. Extra options (e.g. a stub call resolver) are appended.
func newResolver(t *testing.T, opts ...Option) *Resolver {
	t.Helper()
	base := []Option{WithSpecsRoot(corpusRoot(t))}
	r, err := NewResolver(openRegistry(t), append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

// recallStub is an httptest server modeling the Tesseract recall
// endpoint behind the recap/memory call vars. It records whether it was
// hit so a test can assert the call path ran.
type recallStub struct {
	server *httptest.Server
	hits   int
}

func newRecallStub(t *testing.T) *recallStub {
	t.Helper()
	s := &recallStub{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.hits++
		_, _ = w.Write([]byte("- recalled: a session-close note"))
	}))
	t.Cleanup(s.server.Close)
	return s
}

// stubCallResolver returns a fixed value for every http call. It models a
// reachable recall endpoint without depending on a real ${TESSERACT_URL}.
type stubCallResolver struct {
	value any
	hits  int
	err   error
}

func (s *stubCallResolver) ResolveCall(_ context.Context, _ agentlaunch.VarCallRef) (any, error) {
	s.hits++
	if s.err != nil {
		return nil, s.err
	}
	return s.value, nil
}

// TestResolve_RealLaunches resolves several real launch ids from the S5
// corpus and asserts the assembled LaunchPlan is Validate()-clean with the
// expected identity fields.
func TestResolve_RealLaunches(t *testing.T) {
	// A reachable recall endpoint so the call vars resolve cleanly; the
	// cmd vars (git) degrade harmlessly under on_error: warn.
	stub := &stubCallResolver{value: "recalled context"}
	r := newResolver(t, WithCallResolver(stub))

	cases := []struct {
		launchID     string
		wantProject  string
		wantWorkdir  string
		wantProvider string
		wantRuntime  agentlaunch.RuntimeKind
		wantAgent    string
		wantWSMode   agentlaunch.WorkspaceMode
	}{
		{
			launchID:     "tether-claude",
			wantProject:  "tether",
			wantWorkdir:  "~/dev/hollis-labs/apps/tether",
			wantProvider: "claude",
			wantRuntime:  agentlaunch.RuntimeStreamingStdio,
			wantAgent:    "general",
			wantWSMode:   agentlaunch.WorkspacePersistent, // isolation: hybrid
		},
		{
			launchID:     "nanite-claude-stream",
			wantProject:  "nanite",
			wantWorkdir:  "~/dev/hollis-labs/apps/nanite",
			wantProvider: "claude",
			wantRuntime:  agentlaunch.RuntimeStreamingStdio,
			wantAgent:    "general",
			wantWSMode:   agentlaunch.WorkspacePersistent,
		},
		{
			// Repointed 2026-09-12 from the codex-cli runner to
			// codex-app-server: codex-cli declares no command and no
			// binary auto-detects, so this launch could not start at
			// all. The runtime moves with it — codex-cli was a
			// single-turn subprocess, app-server is the long-lived
			// JSON-RPC daemon.
			launchID:     "agent-mux-codex-launch",
			wantProject:  "agent-mux",
			wantWorkdir:  "~/dev/hollis-labs/apps/agent-mux",
			wantProvider: "codex",
			wantRuntime:  agentlaunch.RuntimeJsonRpcStdio,
			wantAgent:    "general",
			wantWSMode:   agentlaunch.WorkspacePersistent,
		},
		{
			launchID: "tether-minimum",
			// Minimum bag supplies no `project`; the resolver derives it
			// from the work_dir basename to satisfy the LaunchPlan
			// Project.ID requirement.
			wantProject: "tether",
			// The bag's work_dir is a literal ~-path; the spec engine
			// does not expand it (input values pass through verbatim),
			// so the plan carries the literal token.
			wantWorkdir:  "~/dev/hollis-labs/apps/tether",
			wantProvider: "claude",
			wantRuntime:  agentlaunch.RuntimeStreamingStdio,
			wantAgent:    "general", // agent defaults to "general"
			wantWSMode:   agentlaunch.WorkspacePersistent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.launchID, func(t *testing.T) {
			plan, err := r.Resolve(tc.launchID, agentlaunch.PolicyError)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.launchID, err)
			}
			if err := plan.Validate(); err != nil {
				t.Fatalf("assembled plan is not Validate()-clean: %v", err)
			}
			if plan.Project.ID != tc.wantProject {
				t.Errorf("Project.ID = %q, want %q", plan.Project.ID, tc.wantProject)
			}
			if plan.Workspace.Workdir != tc.wantWorkdir {
				t.Errorf("Workspace.Workdir = %q, want %q", plan.Workspace.Workdir, tc.wantWorkdir)
			}
			if plan.Workspace.Mode != tc.wantWSMode {
				t.Errorf("Workspace.Mode = %q, want %q", plan.Workspace.Mode, tc.wantWSMode)
			}
			if plan.Provider.ID != tc.wantProvider {
				t.Errorf("Provider.ID = %q, want %q", plan.Provider.ID, tc.wantProvider)
			}
			if plan.Runtime != tc.wantRuntime {
				t.Errorf("Runtime = %q, want %q", plan.Runtime, tc.wantRuntime)
			}
			if plan.Agent.ID != tc.wantAgent {
				t.Errorf("Agent.ID = %q, want %q", plan.Agent.ID, tc.wantAgent)
			}
			if plan.BootProfile.Inline == nil || plan.BootProfile.Inline.BootContent == "" {
				t.Errorf("plan has no inline boot content")
			}
		})
	}
}

// TestResolve_UnresolvableLaunchID asserts an unknown launch id is a
// precise ErrLaunchNotFound.
func TestResolve_UnresolvableLaunchID(t *testing.T) {
	r := newResolver(t, WithCallResolver(&stubCallResolver{value: ""}))
	_, err := r.Resolve("no-such-launch", agentlaunch.PolicyError)
	if !errors.Is(err, ErrLaunchNotFound) {
		t.Fatalf("Resolve(unknown) error = %v, want ErrLaunchNotFound", err)
	}
}

// TestResolve_UnresolvableRunner asserts a launch whose runner has no
// runtime-binding record is a HARD ErrRuntimeBindingNotFound. The fixture
// catalog deliberately omits a provider, and a synthetic bag references
// it; here we drive it via a corpus launch against a catalog with the
// runner removed.
func TestResolve_UnresolvableRunner(t *testing.T) {
	// Build a registry over a catalog that has agents but NO providers,
	// so every runner is unresolvable.
	reg, err := launchresolve.OpenAt(launchresolve.Options{
		CatalogRoot: filepath.Join("testdata", "catalog-no-providers"),
	})
	if err != nil {
		t.Fatalf("open no-providers registry: %v", err)
	}
	r, err := NewResolver(reg,
		WithSpecsRoot(corpusRoot(t)),
		WithCallResolver(&stubCallResolver{value: ""}))
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	_, err = r.Resolve("tether-claude", agentlaunch.PolicyError)
	if !errors.Is(err, launchresolve.ErrRuntimeBindingNotFound) {
		t.Fatalf("Resolve with no runner error = %v, want ErrRuntimeBindingNotFound", err)
	}
}

// TestResolve_OfflineDegraded asserts D1: a launch still resolves when the
// recall endpoint is unreachable. The default http CallResolver dials a
// dead address; every call var is on_error: warn so it degrades to empty
// and the plan is still produced and Validate()-clean.
func TestResolve_OfflineDegraded(t *testing.T) {
	// No WithCallResolver override -> the real httpCallResolver. With
	// TESSERACT_URL unset the call target keeps its ${...} reference and
	// the call fails cleanly; on_error: warn degrades the var.
	t.Setenv("TESSERACT_URL", "")
	r := newResolver(t)

	plan, err := r.Resolve("tether-claude", agentlaunch.PolicyError)
	if err != nil {
		t.Fatalf("offline Resolve must still succeed, got: %v", err)
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("offline plan not Validate()-clean: %v", err)
	}
	body := plan.BootProfile.Inline.BootContent
	// The boot body still renders; the degraded recap/memory vars render
	// as empty under their section headers rather than blocking the launch.
	if !strings.Contains(body, "## 3. Resume Now") {
		t.Errorf("degraded boot body missing the recap section")
	}
	if !strings.Contains(body, "Tether") {
		t.Errorf("degraded boot body missing identity material")
	}
}

// TestResolve_OfflineDegraded_DeadEndpoint exercises the call path with a
// syntactically-valid but unreachable endpoint, confirming a connection
// failure (not just an unexpanded env ref) still degrades cleanly.
func TestResolve_OfflineDegraded_DeadEndpoint(t *testing.T) {
	// Point TESSERACT_URL at a closed port: the http call dials, fails,
	// and on_error: warn degrades the var.
	t.Setenv("TESSERACT_URL", "http://127.0.0.1:1")
	r := newResolver(t)

	plan, err := r.Resolve("nanite-claude-stream", agentlaunch.PolicyError)
	if err != nil {
		t.Fatalf("dead-endpoint Resolve must still succeed, got: %v", err)
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("dead-endpoint plan not Validate()-clean: %v", err)
	}
}

// TestResolve_RecallEndpointReachable confirms the call path actually runs
// when the recall endpoint IS reachable: the stub server records a hit and
// the recalled text lands in the boot body.
func TestResolve_RecallEndpointReachable(t *testing.T) {
	stub := newRecallStub(t)
	t.Setenv("TESSERACT_URL", stub.server.URL)
	r := newResolver(t)

	plan, err := r.Resolve("tether-claude", agentlaunch.PolicyError)
	if err != nil {
		t.Fatalf("Resolve with reachable recall: %v", err)
	}
	if stub.hits == 0 {
		t.Fatalf("recall endpoint was never called")
	}
	if !strings.Contains(plan.BootProfile.Inline.BootContent, "session-close note") {
		t.Errorf("recalled text did not land in the boot body")
	}
}

// TestResolve_InteractiveFrontEnd confirms the interactive front-end maps
// to LaunchInteractive and resolves the same corpus launch.
func TestResolve_InteractiveFrontEnd(t *testing.T) {
	r := newResolver(t, WithCallResolver(&stubCallResolver{value: "ctx"}))
	plan, err := r.Resolve("tether-claude", agentlaunch.PolicyCollect)
	if err != nil {
		t.Fatalf("Resolve interactive: %v", err)
	}
	if plan.Mode != agentlaunch.LaunchInteractive {
		t.Errorf("Mode = %q, want %q", plan.Mode, agentlaunch.LaunchInteractive)
	}
}

// TestResolve_UnauthorizedTrustToken asserts the TrustAuthorizer is
// fail-closed: a gated var source carrying a token outside the catalog
// allow-list produces a permanent var-resolution failure that aborts the
// launch.
func TestResolve_UnauthorizedTrustToken(t *testing.T) {
	denyAll := agentlaunch.TrustAuthorizerFunc(
		func(_ context.Context, varName string, _ agentlaunch.VarSource) (agentlaunch.TrustDecision, error) {
			return agentlaunch.TrustDecision{Allowed: false, Reason: "test deny"}, nil
		})
	r := newResolver(t,
		WithCallResolver(&stubCallResolver{value: ""}),
		WithTrustAuthorizer(denyAll))

	_, err := r.Resolve("tether-claude", agentlaunch.PolicyError)
	if err == nil {
		t.Fatalf("Resolve must fail when a gated source is denied")
	}
	if !agentlaunch.IsPermanentVarError(err) {
		t.Fatalf("denied gated source must be a permanent var error, got: %v", err)
	}
}

// TestCatalogTrustAuthorizer covers the catalog authorizer directly:
// known tokens allowed, unknown/empty denied.
func TestCatalogTrustAuthorizer(t *testing.T) {
	auth := catalogTrustAuthorizer{}
	cases := []struct {
		token   string
		allowed bool
	}{
		{"catalog-recall-endpoint", true},
		{"catalog-git-readonly", true},
		{"catalog-skill-index", true},
		{"some-rogue-token", false},
		{"", false},
	}
	for _, tc := range cases {
		src := agentlaunch.VarSource{
			Kind: agentlaunch.VarSourceCmd,
			Cmd:  &agentlaunch.VarCmdRef{Gate: agentlaunch.TrustGate{Trust: tc.token}},
		}
		dec, err := auth.Authorize(context.Background(), "v", src)
		if err != nil {
			t.Fatalf("Authorize(%q): %v", tc.token, err)
		}
		if dec.Allowed != tc.allowed {
			t.Errorf("Authorize(%q).Allowed = %v, want %v", tc.token, dec.Allowed, tc.allowed)
		}
	}
}

// TestNewResolver_DefaultSpecsRoot confirms the default specsRoot is
// derived under the home directory and is not the testdata path.
func TestNewResolver_DefaultSpecsRoot(t *testing.T) {
	r, err := NewResolver(openRegistry(t))
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if !strings.HasSuffix(r.SpecsRoot(), DefaultSpecsRoot) {
		t.Errorf("default SpecsRoot = %q, want suffix %q", r.SpecsRoot(), DefaultSpecsRoot)
	}
	if strings.Contains(r.SpecsRoot(), "testdata") {
		t.Errorf("default SpecsRoot must not point at testdata: %q", r.SpecsRoot())
	}
}
