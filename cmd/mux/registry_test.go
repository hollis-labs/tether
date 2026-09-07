package main

// registry_test.go — integration coverage for `mux registry ...` (T-v060-01-07).
//
// Fixture choice: we stand up an in-process httptest.Server hosting the
// real api.NewHandler over a real *registry.Service backed by an
// in-memory SQLite. The CLI's registryClientFactory is then pointed at
// that server's URL, exercising the full client → API → service →
// storage stack without spinning up a real daemon (no PID file, no UDS
// socket, no parallel-test contention).
//
// This is the v1-fallback shape the sprint spec calls out: same
// coverage as a daemon fixture but with no listener-port conflicts on
// parallel runs. The trade-off is that we don't exercise
// loadDaemonConfig / unix-socket dialing — both are covered by other
// tests in the cmd/mux package and aren't on the registry critical
// path.
//
// Per-test isolation: each test fn uses its own httptest.Server +
// in-memory DB, so state never leaks between cases. resetRegistryFlags()
// runs in t.Cleanup so flag globals don't leak either.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"

	"net/http"
	"net/http/httptest"
)

// fixture bundles the running httptest server + the service handle so
// individual tests can either seed via the service directly or drive
// through the CLI / RegistryClient.
type fixture struct {
	t   *testing.T
	srv *httptest.Server
	svc *registry.Service
}

// newFixture spins up a fresh service + httptest.Server and rebinds
// registryClientFactory so any CLI RunE invoked during the test hits
// it. ServiceOptions (e.g. file-resolver registration) pass through.
// Also registers a flag-reset cleanup so flag globals don't bleed
// across tests in the run order.
func newFixture(t *testing.T, opts ...registry.ServiceOption) *fixture {
	t.Helper()
	t.Cleanup(resetRegistryFlags)
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	storage := registry.NewStorage(db)
	svc := registry.NewService(storage, opts...)
	h := api.NewHandler(api.Deps{Registry: svc})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// Override the client factory so RunE calls hit our httptest
	// server. client.New accepts a "tcp:host:port" listen address and
	// daemon.BaseURL turns that into "http://host:port" — matching
	// httptest.NewServer's URL shape exactly.
	prevFactory := registryClientFactory
	registryClientFactory = func() (*client.Client, error) {
		hostport := strings.TrimPrefix(srv.URL, "http://")
		return client.New("tcp:" + hostport), nil
	}
	t.Cleanup(func() { registryClientFactory = prevFactory })

	return &fixture{t: t, srv: srv, svc: svc}
}

// seedAgent inserts an agent through the service so a test has a
// known URN to operate on. Returns the canonical Profile.
func (f *fixture) seedAgent(displayName, role string) registry.Profile {
	f.t.Helper()
	p, err := f.svc.Register(context.Background(), registry.KindAgent, registry.Profile{
		DisplayName:   displayName,
		Role:          role,
		LastUpdatedBy: "tester",
	})
	if err != nil {
		f.t.Fatalf("seed agent: %v", err)
	}
	return p
}

// writeFile is a tiny helper that writes a tempfile under t.TempDir
// and returns its path. Used to build --file inputs.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// captureRegistryStdout reroutes os.Stdout for the duration of fn.
// Local fork of agents_test.go's helper so the registry tests are
// self-contained.
func captureRegistryStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	fn()
	_ = w.Close()
	<-done
	os.Stdout = old
	return buf.String()
}

// ─── Register ──────────────────────────────────────────────────────────────

func TestRegistry_RegisterAgent_Happy(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "agent.yaml", `display_name: Alpha
role: implementer
last_updated_by: tester
capabilities: [go, sqlite]
`)
	registerKind = "agent"
	registerFile = path

	out := captureRegistryStdout(t, func() {
		if err := registryRegisterCmd.RunE(registryRegisterCmd, nil); err != nil {
			t.Fatalf("register: %v", err)
		}
	})
	if !strings.Contains(out, "kind:            agent") {
		t.Errorf("output missing kind: %s", out)
	}
	if !strings.Contains(out, "display_name:    Alpha") {
		t.Errorf("output missing display_name: %s", out)
	}
	if !strings.Contains(out, "capabilities:    [go, sqlite]") {
		t.Errorf("output missing capabilities: %s", out)
	}
	// Server must have minted a urn.
	if !strings.Contains(out, "msg://agent/agent-mux/agt_") {
		t.Errorf("output missing minted URN: %s", out)
	}
	_ = f // referenced only for cleanup wiring; suppress unused-var lint
}

func TestRegistry_RegisterAgent_JSONInput(t *testing.T) {
	_ = newFixture(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "agent.json", `{"display_name":"JSONAlpha","role":"r","last_updated_by":"t"}`)
	registerKind = "agent"
	registerFile = path

	out := captureRegistryStdout(t, func() {
		if err := registryRegisterCmd.RunE(registryRegisterCmd, nil); err != nil {
			t.Fatalf("register: %v", err)
		}
	})
	if !strings.Contains(out, "JSONAlpha") {
		t.Errorf("JSON input not honored: %s", out)
	}
}

func TestRegistry_RegisterAgent_PrintURNOnly(t *testing.T) {
	_ = newFixture(t)
	dir := t.TempDir()
	path := writeFile(t, dir, "agent.yaml", "display_name: Pipe\nrole: r\nlast_updated_by: tester\n")
	registerKind = "agent"
	registerFile = path
	registerPrintURN = true

	out := captureRegistryStdout(t, func() {
		if err := registryRegisterCmd.RunE(registryRegisterCmd, nil); err != nil {
			t.Fatalf("register: %v", err)
		}
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("--print-urn-only should emit a single line; got %d: %q", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "msg://agent/agent-mux/agt_") {
		t.Errorf("single line is not a urn: %q", lines[0])
	}
}

func TestRegistry_Register_MalformedFile_Exit2(t *testing.T) {
	_ = newFixture(t)
	dir := t.TempDir()
	// Trailing-tab in YAML is a hard parse error; JSON sniff treats
	// the leading character (`!`) as "not JSON" so it falls into the
	// YAML branch and rejects.
	path := writeFile(t, dir, "broken.yaml", "!!! not valid yaml: [\n")
	registerKind = "agent"
	registerFile = path

	err := registryRegisterCmd.RunE(registryRegisterCmd, nil)
	if err == nil {
		t.Fatal("expected error on malformed file")
	}
	assertExitCode(t, err, 2)
	resetRegistryFlags()
}

func TestRegistry_Register_MissingFlags_Exit2(t *testing.T) {
	_ = newFixture(t)
	// No --kind, no --file
	err := registryRegisterCmd.RunE(registryRegisterCmd, nil)
	if err == nil {
		t.Fatal("expected error for missing flags")
	}
	assertExitCode(t, err, 2)
	resetRegistryFlags()
}

// ─── Lookup ────────────────────────────────────────────────────────────────

func TestRegistry_Lookup_Happy(t *testing.T) {
	f := newFixture(t)
	seed := f.seedAgent("Looked-Up", "reviewer")

	out := captureRegistryStdout(t, func() {
		if err := registryLookupCmd.RunE(registryLookupCmd, []string{seed.URN}); err != nil {
			t.Fatalf("lookup: %v", err)
		}
	})
	if !strings.Contains(out, "Looked-Up") {
		t.Errorf("lookup output missing display_name: %s", out)
	}
	if !strings.Contains(out, "role:            reviewer") {
		t.Errorf("lookup output missing role: %s", out)
	}
}

func TestRegistry_Lookup_JSON(t *testing.T) {
	f := newFixture(t)
	seed := f.seedAgent("Json-Boy", "r")
	lookupJSON = true

	out := captureRegistryStdout(t, func() {
		if err := registryLookupCmd.RunE(registryLookupCmd, []string{seed.URN}); err != nil {
			t.Fatalf("lookup: %v", err)
		}
	})
	resetRegistryFlags()
	var got registry.Profile
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode json output: %v: %s", err, out)
	}
	if got.URN != seed.URN {
		t.Errorf("urn = %q, want %q", got.URN, seed.URN)
	}
}

func TestRegistry_Lookup_NotFound_Exit1(t *testing.T) {
	_ = newFixture(t)
	err := registryLookupCmd.RunE(registryLookupCmd, []string{"msg://agent/agent-mux/agt_nonexistent"})
	if err == nil {
		t.Fatal("expected ErrNotFound")
	}
	assertExitCode(t, err, 1)
}

// ─── Search ────────────────────────────────────────────────────────────────

func TestRegistry_Search_FiltersByRole(t *testing.T) {
	f := newFixture(t)
	f.seedAgent("Aa", "reviewer")
	f.seedAgent("Bb", "reviewer")
	f.seedAgent("Cc", "planner")

	searchKind = "agent"
	searchRole = "reviewer"

	out := captureRegistryStdout(t, func() {
		if err := registrySearchCmd.RunE(registrySearchCmd, nil); err != nil {
			t.Fatalf("search: %v", err)
		}
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 rows, got %d: %q", len(lines), out)
	}
	for _, line := range lines {
		if !strings.Contains(line, "role=reviewer") {
			t.Errorf("row missing role=reviewer: %q", line)
		}
	}
}

func TestRegistry_Search_HidesDeprecatedByDefault(t *testing.T) {
	f := newFixture(t)
	active := f.seedAgent("StaysActive", "r")
	dep := f.seedAgent("DeadAgent", "r")
	if _, err := f.svc.Deregister(context.Background(), dep.URN); err != nil {
		t.Fatalf("deregister: %v", err)
	}

	// Default search excludes deprecated.
	searchKind = "agent"
	out := captureRegistryStdout(t, func() {
		if err := registrySearchCmd.RunE(registrySearchCmd, nil); err != nil {
			t.Fatalf("search default: %v", err)
		}
	})
	if !strings.Contains(out, active.URN) {
		t.Errorf("default search missing active row: %s", out)
	}
	if strings.Contains(out, dep.URN) {
		t.Errorf("default search should have excluded deprecated: %s", out)
	}
	resetRegistryFlags()

	// --status deprecated brings them back.
	searchKind = "agent"
	searchStatus = "deprecated"
	out = captureRegistryStdout(t, func() {
		if err := registrySearchCmd.RunE(registrySearchCmd, nil); err != nil {
			t.Fatalf("search status=deprecated: %v", err)
		}
	})
	if !strings.Contains(out, dep.URN) {
		t.Errorf("status=deprecated search missing dep row: %s", out)
	}
}

func TestRegistry_Search_MissingKind_Exit2(t *testing.T) {
	_ = newFixture(t)
	err := registrySearchCmd.RunE(registrySearchCmd, nil)
	if err == nil {
		t.Fatal("expected error on missing --kind")
	}
	assertExitCode(t, err, 2)
	resetRegistryFlags()
}

// ─── UpdateSelf ────────────────────────────────────────────────────────────

func TestRegistry_UpdateSelf_Happy(t *testing.T) {
	f := newFixture(t)
	seed := f.seedAgent("Original", "r")

	dir := t.TempDir()
	path := writeFile(t, dir, "patch.yaml", `last_updated_by: tester
display_name: Renamed
capabilities: [first, second]
`)
	updateSelfFile = path

	out := captureRegistryStdout(t, func() {
		if err := registryUpdateSelfCmd.RunE(registryUpdateSelfCmd, []string{seed.URN}); err != nil {
			t.Fatalf("update-self: %v", err)
		}
	})
	if !strings.Contains(out, "Renamed") {
		t.Errorf("output missing renamed display_name: %s", out)
	}
	if !strings.Contains(out, "[first, second]") {
		t.Errorf("output missing replaced capabilities: %s", out)
	}
}

func TestRegistry_UpdateSelf_MissingLastUpdatedBy_Exit2(t *testing.T) {
	f := newFixture(t)
	seed := f.seedAgent("Subject", "r")

	dir := t.TempDir()
	path := writeFile(t, dir, "patch.yaml", "display_name: NoCaller\n")
	updateSelfFile = path

	err := registryUpdateSelfCmd.RunE(registryUpdateSelfCmd, []string{seed.URN})
	if err == nil {
		t.Fatal("expected validation error")
	}
	assertExitCode(t, err, 2)
}

// ─── Deregister ───────────────────────────────────────────────────────────

func TestRegistry_Deregister_Happy(t *testing.T) {
	f := newFixture(t)
	seed := f.seedAgent("DepMe", "r")

	out := captureRegistryStdout(t, func() {
		if err := registryDeregisterCmd.RunE(registryDeregisterCmd, []string{seed.URN}); err != nil {
			t.Fatalf("deregister: %v", err)
		}
	})
	want := "deregistered: " + seed.URN + "  status=deprecated"
	if !strings.Contains(out, want) {
		t.Errorf("output missing confirmation %q: %s", want, out)
	}
}

func TestRegistry_Deregister_NotFound_Exit1(t *testing.T) {
	_ = newFixture(t)
	err := registryDeregisterCmd.RunE(registryDeregisterCmd, []string{"msg://agent/agent-mux/agt_doesnotexist"})
	if err == nil {
		t.Fatal("expected not-found")
	}
	assertExitCode(t, err, 1)
}

// ─── Sync ─────────────────────────────────────────────────────────────────

func TestRegistry_Sync_NoCallback_NoOp(t *testing.T) {
	f := newFixture(t)
	seed := f.seedAgent("NoCB", "r")

	out := captureRegistryStdout(t, func() {
		if err := registrySyncCmd.RunE(registrySyncCmd, []string{seed.URN}); err != nil {
			t.Fatalf("sync: %v", err)
		}
	})
	want := "no-op: " + seed.URN + " has no callback"
	if !strings.Contains(out, want) {
		t.Errorf("expected %q, got %q", want, out)
	}
}

func TestRegistry_Sync_FileCallback_Refreshes(t *testing.T) {
	tmp := t.TempDir()
	fxPath := filepath.Join(tmp, "agent.yaml")
	if err := os.WriteFile(fxPath, []byte(`{
  "display_name": "After-Sync",
  "role": "after",
  "capabilities": ["fresh"]
}`), 0o600); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	resolver, err := registry.NewFileResolver(tmp)
	if err != nil {
		t.Fatalf("file resolver: %v", err)
	}
	f := newFixture(t, registry.WithResolver(resolver))

	p, err := f.svc.Register(context.Background(), registry.KindAgent, registry.Profile{
		DisplayName:   "Pre-Sync",
		Role:          "before",
		LastUpdatedBy: "tester",
		Callback: &registry.Callback{
			Scheme: "file",
			Target: "file://" + fxPath,
		},
	})
	if err != nil {
		t.Fatalf("seed with callback: %v", err)
	}

	out := captureRegistryStdout(t, func() {
		if err := registrySyncCmd.RunE(registrySyncCmd, []string{p.URN}); err != nil {
			t.Fatalf("sync: %v", err)
		}
	})
	if !strings.Contains(out, "After-Sync") {
		t.Errorf("sync output missing refreshed display_name: %s", out)
	}
	if !strings.Contains(out, "cached_at:") {
		t.Errorf("sync output missing cached_at bump: %s", out)
	}
}

// ─── Daemon-unreachable mapping ───────────────────────────────────────────

func TestRegistry_DaemonUnreachable_Exit3(t *testing.T) {
	// Bring up a fixture, then close its server so the next call
	// hits a connect-refused. The factory keeps the (now-dead) URL,
	// surfacing ErrDaemonUnreachable from the client.
	f := newFixture(t)
	f.srv.Close()

	err := registryLookupCmd.RunE(registryLookupCmd, []string{"msg://agent/agent-mux/agt_anything01"})
	if err == nil {
		t.Fatal("expected unreachable error")
	}
	assertExitCode(t, err, 3)
}

// ─── JSON output (search) ─────────────────────────────────────────────────

func TestRegistry_Search_JSONOutput(t *testing.T) {
	f := newFixture(t)
	a := f.seedAgent("JZ", "r")

	searchKind = "agent"
	searchJSON = true
	out := captureRegistryStdout(t, func() {
		if err := registrySearchCmd.RunE(registrySearchCmd, nil); err != nil {
			t.Fatalf("search: %v", err)
		}
	})
	resetRegistryFlags()
	var rows []registry.Profile
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode json: %v: %s", err, out)
	}
	if len(rows) != 1 || rows[0].URN != a.URN {
		t.Errorf("unexpected rows: %+v", rows)
	}
}

// ─── help text contract ───────────────────────────────────────────────────

// TestRegistry_UpdateSelfHelpDocsMergeSemantics enforces the
// acceptance criterion: `--help` on update-self must document the
// partial-merge model. We assert the four required pillars (scalar
// pointers, array shorthand vs explicit, empty-value no-op,
// last_updated_by required) are present.
func TestRegistry_UpdateSelfHelpDocsMergeSemantics(t *testing.T) {
	long := registryUpdateSelfCmd.Long
	for _, want := range []string{
		"Scalar",
		"Shorthand",
		"replace|append|remove",
		"no-op",
		"last_updated_by is REQUIRED",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("update-self Long missing %q; full text:\n%s", want, long)
		}
	}
}

// ─── bootstrap ─────────────────────────────────────────────────────────────

// newBootstrapFixture is a variant of newFixture that also wires
// api.Deps.RegistryCatalogRoot. The catalog root is populated with a
// minimal fixture (1 agent, 1 project) so the bootstrap CLI command has
// something to land. Returns the fixture + the catalog root so tests
// can mutate YAML files between passes.
func newBootstrapFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	t.Cleanup(resetRegistryFlags)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "agents", "alpha.yaml"),
		[]byte("id: alpha\nname: Alpha Original\nroles: [implementer]\nskills: [go]\n"), 0o600); err != nil {
		t.Fatalf("write alpha.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "projects", "tether.yaml"),
		[]byte("id: tether\nname: Tether\nrepo_root: /repos/tether\ntracking_root: /tracking/tether\n"), 0o600); err != nil {
		t.Fatalf("write tether.yaml: %v", err)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	storage := registry.NewStorage(db)
	svc := registry.NewService(storage)
	h := api.NewHandler(api.Deps{
		Registry:            svc,
		RegistryCatalogRoot: root,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	prevFactory := registryClientFactory
	registryClientFactory = func() (*client.Client, error) {
		hostport := strings.TrimPrefix(srv.URL, "http://")
		return client.New("tcp:" + hostport), nil
	}
	t.Cleanup(func() { registryClientFactory = prevFactory })

	return &fixture{t: t, srv: srv, svc: svc}, root
}

func TestRegistry_Bootstrap_FirstRun_ImportsAndPrintsCounts(t *testing.T) {
	_, _ = newBootstrapFixture(t)

	out := captureRegistryStdout(t, func() {
		if err := registryBootstrapCmd.RunE(registryBootstrapCmd, nil); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
	})
	if !strings.Contains(out, "imported:  2") {
		t.Errorf("expected imported=2; got: %s", out)
	}
	if !strings.Contains(out, "skipped:   0") {
		t.Errorf("expected skipped=0; got: %s", out)
	}
	if !strings.Contains(out, "errors:    0") {
		t.Errorf("expected errors=0; got: %s", out)
	}
}

func TestRegistry_Bootstrap_SecondRun_NoOpSkips(t *testing.T) {
	_, _ = newBootstrapFixture(t)

	// First pass — imports.
	if err := registryBootstrapCmd.RunE(registryBootstrapCmd, nil); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	// Second pass — should report 0 imported, all skipped.
	out := captureRegistryStdout(t, func() {
		if err := registryBootstrapCmd.RunE(registryBootstrapCmd, nil); err != nil {
			t.Fatalf("second bootstrap: %v", err)
		}
	})
	if !strings.Contains(out, "imported:  0") {
		t.Errorf("expected imported=0 on re-run; got: %s", out)
	}
	if !strings.Contains(out, "skipped:   2") {
		t.Errorf("expected skipped=2 on re-run; got: %s", out)
	}
	if !strings.Contains(out, "refreshed: 0") {
		t.Errorf("expected refreshed=0 without --force; got: %s", out)
	}
}

func TestRegistry_Bootstrap_Force_RefreshesEdits(t *testing.T) {
	_, root := newBootstrapFixture(t)

	// First pass — imports.
	if err := registryBootstrapCmd.RunE(registryBootstrapCmd, nil); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	// Mutate the agent YAML's display_name.
	if err := os.WriteFile(filepath.Join(root, "agents", "alpha.yaml"),
		[]byte("id: alpha\nname: Alpha Refreshed\nroles: [implementer]\nskills: [go, sqlite]\n"), 0o600); err != nil {
		t.Fatalf("rewrite alpha.yaml: %v", err)
	}

	// Run with --force.
	bootstrapForce = true
	out := captureRegistryStdout(t, func() {
		if err := registryBootstrapCmd.RunE(registryBootstrapCmd, nil); err != nil {
			t.Fatalf("force bootstrap: %v", err)
		}
	})
	if !strings.Contains(out, "imported:  0") {
		t.Errorf("expected imported=0 on --force; got: %s", out)
	}
	// --force refreshes every existing row (idempotent UpdateSelf is
	// safe on unchanged rows); 2 rows in the fixture => refreshed=2.
	if !strings.Contains(out, "refreshed: 2") {
		t.Errorf("expected refreshed=2 on --force; got: %s", out)
	}

	// Lookup via the CLI to verify display_name changed.
	searchKind = "agent"
	out = captureRegistryStdout(t, func() {
		if err := registrySearchCmd.RunE(registrySearchCmd, nil); err != nil {
			t.Fatalf("search: %v", err)
		}
	})
	if !strings.Contains(out, "Alpha Refreshed") {
		t.Errorf("display_name not refreshed; search output: %s", out)
	}
	if strings.Contains(out, "Alpha Original") {
		t.Errorf("stale display_name still present; search output: %s", out)
	}
}

// ─── bindings (T08) ─────────────────────────────────────────────────────────

func TestRegistryBindings_LeaseCurrentListRenewRevoke_Roundtrip(t *testing.T) {
	_ = newFixture(t)
	target := "msg://agent/agent-mux/worker"

	bindingLeaseTargetURN = target
	bindingLeaseSessionID = "bridge-1"
	bindingLeaseHostID = "host-1"
	bindingLeaseAttemptID = "attempt-1"
	bindingLeaseCaps = []string{"pull-only"}

	out := captureRegistryStdout(t, func() {
		if err := bindingsLeaseCmd.RunE(bindingsLeaseCmd, nil); err != nil {
			t.Fatalf("lease: %v", err)
		}
	})
	if !strings.Contains(out, "target_urn:  "+target) || !strings.Contains(out, "visibility:  published-local") {
		t.Fatalf("lease output missing expected fields: %s", out)
	}
	// Extract the minted binding id from the pretty-printed output.
	var bindingID string
	for _, line := range strings.Split(out, "\n") {
		if id, ok := strings.CutPrefix(line, "id:          "); ok {
			bindingID = id
		}
	}
	if bindingID == "" {
		t.Fatalf("could not find binding id in lease output: %s", out)
	}

	out = captureRegistryStdout(t, func() {
		if err := bindingsCurrentCmd.RunE(bindingsCurrentCmd, nil); err != nil {
			t.Fatalf("current: %v", err)
		}
	})
	if !strings.Contains(out, "id:          "+bindingID) {
		t.Fatalf("current output = %s, want binding id %s", out, bindingID)
	}

	bindingLeaseTTL = 3600
	out = captureRegistryStdout(t, func() {
		if err := bindingsRenewCmd.RunE(bindingsRenewCmd, []string{bindingID}); err != nil {
			t.Fatalf("renew: %v", err)
		}
	})
	if !strings.Contains(out, "lease_expires_at:") {
		t.Fatalf("renew output = %s, want a lease_expires_at line", out)
	}

	out = captureRegistryStdout(t, func() {
		if err := bindingsRevokeCmd.RunE(bindingsRevokeCmd, []string{bindingID}); err != nil {
			t.Fatalf("revoke: %v", err)
		}
	})
	if !strings.Contains(out, "revoked: "+bindingID) {
		t.Fatalf("revoke output = %s, want confirmation", out)
	}

	out = captureRegistryStdout(t, func() {
		if err := bindingsListCmd.RunE(bindingsListCmd, nil); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	if !strings.Contains(out, "id:          "+bindingID) {
		t.Fatalf("list output = %s, want the revoked binding still listed", out)
	}
}

func TestRegistryBindings_Lease_CannotSupersedeTetherManagedBinding(t *testing.T) {
	f := newFixture(t)
	target := "msg://agent/agent-mux/worker"
	if _, err := f.svc.LeaseBinding(context.Background(), target, "real-session", "local", "real-session", nil, registry.VisibilityPrivateLocal, 0); err != nil {
		t.Fatalf("seed private-local binding: %v", err)
	}

	bindingLeaseTargetURN = target
	bindingLeaseSessionID = "bridge-1"
	bindingLeaseHostID = "host-1"
	bindingLeaseAttemptID = "attempt-1"
	bindingLeaseCaps = []string{"pull-only"}

	err := bindingsLeaseCmd.RunE(bindingsLeaseCmd, nil)
	if err == nil {
		t.Fatal("expected an error superseding a Tether-managed binding")
	}
	assertExitCode(t, err, 4)
}

func TestRegistryBindings_Lease_MissingFlags_Exit2(t *testing.T) {
	_ = newFixture(t)
	err := bindingsLeaseCmd.RunE(bindingsLeaseCmd, nil)
	if err == nil {
		t.Fatal("expected a validation error with no flags set")
	}
	assertExitCode(t, err, 2)
}

// ─── scoped-bindings (T08) ────────────────────────────────────────────────

func TestRegistryScopedBindings_SetResolveRevisions_Roundtrip(t *testing.T) {
	_ = newFixture(t)

	scopedBindingScope = "run-1"
	scopedBindingSlot = "reviewer"
	scopedBindingTargets = []string{"msg://agent/agent-mux/agt_a"}
	scopedBindingCreatedBy = "msg://agent/agent-mux/agt_owner"

	out := captureRegistryStdout(t, func() {
		if err := scopedBindingsSetCmd.RunE(scopedBindingsSetCmd, nil); err != nil {
			t.Fatalf("set: %v", err)
		}
	})
	if !strings.Contains(out, "revision:    1") {
		t.Fatalf("set output missing revision 1: %s", out)
	}

	out = captureRegistryStdout(t, func() {
		if err := scopedBindingsResolveCmd.RunE(scopedBindingsResolveCmd, nil); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	})
	if !strings.Contains(out, "target_urns: msg://agent/agent-mux/agt_a") {
		t.Fatalf("resolve output = %s, want the set target", out)
	}

	scopedBindingSingle = true
	out = captureRegistryStdout(t, func() {
		if err := scopedBindingsResolveCmd.RunE(scopedBindingsResolveCmd, nil); err != nil {
			t.Fatalf("resolve single: %v", err)
		}
	})
	if !strings.Contains(out, "target_urn: msg://agent/agent-mux/agt_a") {
		t.Fatalf("resolve single output = %s, want the single target", out)
	}

	// A second revision, then list history.
	scopedBindingTargets = []string{"msg://agent/agent-mux/agt_b"}
	if err := scopedBindingsSetCmd.RunE(scopedBindingsSetCmd, nil); err != nil {
		t.Fatalf("set 2: %v", err)
	}
	out = captureRegistryStdout(t, func() {
		if err := scopedBindingsRevisionsCmd.RunE(scopedBindingsRevisionsCmd, nil); err != nil {
			t.Fatalf("revisions: %v", err)
		}
	})
	if strings.Count(out, "scope:       run-1") != 2 {
		t.Fatalf("revisions output = %s, want 2 revisions listed", out)
	}
}

func TestRegistryScopedBindings_Set_MissingFlags_Exit2(t *testing.T) {
	_ = newFixture(t)
	if err := scopedBindingsSetCmd.RunE(scopedBindingsSetCmd, nil); err == nil {
		t.Fatal("expected a validation error with no flags set")
	} else {
		assertExitCode(t, err, 2)
	}
}

func TestRegistryScopedBindings_Resolve_NotFound_Exit1(t *testing.T) {
	_ = newFixture(t)
	scopedBindingScope = "nope"
	scopedBindingSlot = "nope"
	err := scopedBindingsResolveCmd.RunE(scopedBindingsResolveCmd, nil)
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	assertExitCode(t, err, 1)
}

// ─── helpers ──────────────────────────────────────────────────────────────

// assertExitCode walks an error chain to find an exitErr and verifies
// its code. Failures dump the chain so the user sees what classifier
// got hit.
func assertExitCode(t *testing.T, err error, want int) {
	t.Helper()
	var ec *exitErr
	if !errors.As(err, &ec) {
		t.Fatalf("err is not *exitErr: %T %v", err, err)
	}
	if ec.code != want {
		t.Errorf("exit code = %d, want %d (err=%v)", ec.code, want, err)
	}
}

// _ enforces the modernc.org/sqlite blank import isn't tree-shaken
// away by goimports — registry.Migrate needs the driver registered.
var _ = http.MethodGet
