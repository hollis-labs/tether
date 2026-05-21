package main

// group_test.go — integration coverage for `mux group ...` (T-v060-05-06).
// Same shape as registry_test.go: a httptest.Server hosts the real
// api.Handler over a live *registry.Service so the full CLI → client
// → API → service stack is exercised without a real daemon.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"

	"net/http/httptest"
)

// groupFixture mirrors fixture in registry_test.go but wires both
// Registry and Groups onto the same *registry.Service (production
// shape) so a single subprocess test can do registry register +
// group lifecycle through one CLI factory.
type groupFixture struct {
	t   *testing.T
	srv *httptest.Server
	svc *registry.Service
}

func newGroupFixture(t *testing.T) *groupFixture {
	t.Helper()
	t.Cleanup(resetRegistryFlags)
	t.Cleanup(resetGroupFlags)
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
	h := api.NewHandler(api.Deps{Registry: svc, Groups: svc})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	prevFactory := registryClientFactory
	registryClientFactory = func() (*client.Client, error) {
		hostport := strings.TrimPrefix(srv.URL, "http://")
		return client.New("tcp:" + hostport), nil
	}
	t.Cleanup(func() { registryClientFactory = prevFactory })

	return &groupFixture{t: t, srv: srv, svc: svc}
}

// seedAgent inserts an agent and returns its URN. Used to give a
// creator URN to group lifecycle tests.
func (f *groupFixture) seedAgent(name string) string {
	f.t.Helper()
	p, err := f.svc.Register(context.Background(), registry.KindAgent, registry.Profile{
		DisplayName:   name,
		LastUpdatedBy: "tester",
	})
	if err != nil {
		f.t.Fatalf("seed agent: %v", err)
	}
	return p.URN
}

// ─── create ───────────────────────────────────────────────────────────

func TestGroupCmd_Create_Happy(t *testing.T) {
	f := newGroupFixture(t)
	creator := f.seedAgent("Owner")
	groupAsFlag = creator
	groupCreateName = "Design Room"
	out := captureRegistryStdout(t, func() {
		if err := groupCreateCmd.RunE(groupCreateCmd, nil); err != nil {
			t.Fatalf("create: %v", err)
		}
	})
	if !strings.Contains(out, "display_name:    Design Room") {
		t.Errorf("output missing display_name: %s", out)
	}
	if !strings.Contains(out, "msg://group/agent-mux/grp_") {
		t.Errorf("output missing minted group URN: %s", out)
	}
}

func TestGroupCmd_Create_MissingAs(t *testing.T) {
	_ = newGroupFixture(t)
	groupCreateName = "Bad"
	err := groupCreateCmd.RunE(groupCreateCmd, nil)
	if err == nil {
		t.Fatal("expected validation error")
	}
	var ee *exitErr
	if !errorsAs(err, &ee) || ee.code != 2 {
		t.Errorf("err=%v want exit code 2", err)
	}
}

// ─── list --mine ──────────────────────────────────────────────────────

func TestGroupCmd_ListMine(t *testing.T) {
	f := newGroupFixture(t)
	owner := f.seedAgent("Owner")
	// Create a group through the CLI so the fixture round-trips identity
	// through the auth surrogate the way real callers would.
	groupAsFlag = owner
	groupCreateName = "MyGrp"
	_ = captureRegistryStdout(t, func() {
		if err := groupCreateCmd.RunE(groupCreateCmd, nil); err != nil {
			t.Fatalf("create: %v", err)
		}
	})
	// Now list.
	resetGroupFlags()
	groupAsFlag = owner
	groupListMine = true
	out := captureRegistryStdout(t, func() {
		if err := groupListCmd.RunE(groupListCmd, nil); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	if !strings.Contains(out, "MyGrp") {
		t.Errorf("list output missing group: %s", out)
	}
}

func TestGroupCmd_ListMissingMine(t *testing.T) {
	_ = newGroupFixture(t)
	err := groupListCmd.RunE(groupListCmd, nil)
	if err == nil {
		t.Fatal("expected validation error")
	}
	var ee *exitErr
	if !errorsAs(err, &ee) || ee.code != 2 {
		t.Errorf("err=%v want exit code 2", err)
	}
}

// ─── show ─────────────────────────────────────────────────────────────

func TestGroupCmd_Show_NotFound_Exit1(t *testing.T) {
	_ = newGroupFixture(t)
	err := groupShowCmd.RunE(groupShowCmd, []string{"msg://group/agent-mux/grp_ghost00000"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ee *exitErr
	if !errorsAs(err, &ee) || ee.code != 1 {
		t.Errorf("err=%v want exit code 1", err)
	}
}

// ─── invite + read + post integration ─────────────────────────────────

func TestGroupCmd_FullRoundtrip(t *testing.T) {
	f := newGroupFixture(t)
	owner := f.seedAgent("Owner")
	bob := f.seedAgent("Bob")

	groupAsFlag = owner
	groupCreateName = "RoundTrip"
	var grpURN string
	out := captureRegistryStdout(t, func() {
		if err := groupCreateCmd.RunE(groupCreateCmd, nil); err != nil {
			t.Fatalf("create: %v", err)
		}
	})
	// Extract the minted URN from output.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "urn:") {
			grpURN = strings.TrimSpace(strings.TrimPrefix(line, "urn:"))
			break
		}
	}
	if grpURN == "" {
		t.Fatalf("could not extract URN from output: %s", out)
	}

	// invite bob.
	resetGroupFlags()
	groupAsFlag = owner
	groupInviteRole = "member"
	if err := groupInviteCmd.RunE(groupInviteCmd, []string{grpURN, bob}); err != nil {
		t.Fatalf("invite: %v", err)
	}

	// post (as owner).
	resetGroupFlags()
	groupAsFlag = owner
	postOut := captureRegistryStdout(t, func() {
		if err := groupPostCmd.RunE(groupPostCmd, []string{grpURN, "hello room"}); err != nil {
			t.Fatalf("post: %v", err)
		}
	})
	if !strings.Contains(postOut, "group_seq=1") {
		t.Errorf("post output missing group_seq: %s", postOut)
	}

	// read (as bob).
	resetGroupFlags()
	groupAsFlag = bob
	readOut := captureRegistryStdout(t, func() {
		if err := groupReadCmd.RunE(groupReadCmd, []string{grpURN}); err != nil {
			t.Fatalf("read: %v", err)
		}
	})
	if !strings.Contains(readOut, "hello room") {
		t.Errorf("read output missing payload: %s", readOut)
	}
}

// ─── archive ──────────────────────────────────────────────────────────

func TestGroupCmd_Archive(t *testing.T) {
	f := newGroupFixture(t)
	owner := f.seedAgent("Owner")
	groupAsFlag = owner
	groupCreateName = "Doomed"
	var urn string
	out := captureRegistryStdout(t, func() {
		if err := groupCreateCmd.RunE(groupCreateCmd, nil); err != nil {
			t.Fatalf("create: %v", err)
		}
	})
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "urn:") {
			urn = strings.TrimSpace(strings.TrimPrefix(line, "urn:"))
			break
		}
	}

	resetGroupFlags()
	groupAsFlag = owner
	archOut := captureRegistryStdout(t, func() {
		if err := groupArchiveCmd.RunE(groupArchiveCmd, []string{urn}); err != nil {
			t.Fatalf("archive: %v", err)
		}
	})
	if !strings.Contains(archOut, "archived:") {
		t.Errorf("archive output unexpected: %s", archOut)
	}
}

// errorsAs is a tiny wrapper around stdlib errors.As that targets
// *exitErr specifically. Keeps test bodies readable without spreading
// errors.As's two-arg dance through each assertion.
func errorsAs(err error, target **exitErr) bool {
	return errors.As(err, target)
}
