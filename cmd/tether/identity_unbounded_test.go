package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func TestIdentityUnboundedCountsAndListsWithoutChangingAnything(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ids := identity.NewStore(db.DB())
	future := time.Now().Add(time.Hour)
	for id, p := range map[string]identity.Principal{
		"s-anon":   {CreatedBy: "", LaunchAttribution: "anonymous_external"},
		"s-legacy": {CreatedBy: ""},
		"s-ok":     {CreatedBy: identity.OperatorID, LaunchAttribution: "principal"},
	} {
		if err = db.CreateSession(store.SessionRow{ID: id, State: "created"}, nil); err != nil {
			t.Fatal(err)
		}
		p.ID, p.Kind, p.SessionID, p.ExpiresAt = "msg://session/local/"+id, "session", id, &future
		if _, err = ids.MintSessionForLaunch(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	list, err := reportUnbounded(context.Background(), &out, ids)
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"2 live unbounded", "anonymous_external", "legacy_unknown_launcher", "s-anon", "s-legacy"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "s-ok") || len(list) != 2 {
		t.Errorf("a bounded credential was listed:\n%s", text)
	}
	// Read-only: running it again lists the same two.
	again, err := ids.UnboundedSessions(context.Background())
	if err != nil || len(again) != 2 {
		t.Fatalf("report changed state: %v %v", again, err)
	}
}

func guardFixture(t *testing.T, unbounded bool) (root string, db *store.Store) {
	t.Helper()
	state := t.TempDir()
	root = filepath.Join(state, "catalog")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(state, "state.db")
	global := "# keep me\ndaemon:\n  listen_addr: unix:/tmp/x.sock\ncatalog:\n  defaults:\n    state_db: " + dbPath + "\nidentity:\n  mode: observe\n"
	if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	db, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ids := identity.NewStore(db.DB())
	future := time.Now().Add(time.Hour)
	mint := func(id, createdBy, attribution string) {
		t.Helper()
		if err = db.CreateSession(store.SessionRow{ID: id, State: "created"}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err = ids.MintSessionForLaunch(context.Background(), identity.Principal{ID: "msg://session/local/" + id, Kind: "session", SessionID: id, CreatedBy: createdBy, LaunchAttribution: attribution, ExpiresAt: &future}); err != nil {
			t.Fatal(err)
		}
	}
	mint("bounded", identity.OperatorID, "principal")
	if unbounded {
		mint("anon", "", "anonymous_external")
		mint("old", "", "")
	}
	return root, db
}

func fileMode(t *testing.T, root string) string {
	t.Helper()
	cat, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return cat.Global.Identity.EffectiveMode()
}

func TestEnforceFlipIsRefusedWhileUnboundedCredentialsStand(t *testing.T) {
	root, db := guardFixture(t, true)
	t.Cleanup(func() { _ = db.Close() })
	var out bytes.Buffer
	_, err := flipMode(context.Background(), &out, root, "global", identity.Enforce, "t", "CW-1 GO", false)
	if err == nil || !strings.Contains(err.Error(), "refusing to switch to enforce") || !strings.Contains(err.Error(), "--revoke-anonymous") {
		t.Fatalf("flip not refused: %v", err)
	}
	// The refusal prints exactly what --revoke-anonymous would hit.
	for _, want := range []string{"2 live unbounded", "anonymous_external", "legacy_unknown_launcher", "anon", "old"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("refusal output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "\nbounded ") {
		t.Errorf("a bounded credential was listed:\n%s", out.String())
	}
	if got := fileMode(t, root); got != "observe" {
		t.Fatalf("mode changed to %q despite the refusal", got)
	}
	if _, statErr := os.Stat(flipLogPath(root)); statErr == nil {
		t.Fatal("a refused flip wrote an audit record")
	}
	left, _ := identity.NewStore(db.DB()).UnboundedSessions(context.Background())
	if len(left) != 2 {
		t.Fatalf("a refused flip revoked credentials: %d left", len(left))
	}
}

func TestEnforceFlipWithRevokeAnonymousRevokesExactlyTheListAndRecordsIt(t *testing.T) {
	root, db := guardFixture(t, true)
	t.Cleanup(func() { _ = db.Close() })
	var out bytes.Buffer
	rec, err := flipMode(context.Background(), &out, root, "global", identity.Enforce, "t", "CW-1 GO", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Evidence, "revoked 2 unbounded") || !strings.Contains(out.String(), "revoked 2") {
		t.Fatalf("revocation not recorded: evidence %q output %q", rec.Evidence, out.String())
	}
	if got := fileMode(t, root); got != "enforce" {
		t.Fatalf("mode %q, want enforce", got)
	}
	ids := identity.NewStore(db.DB())
	if left, _ := ids.UnboundedSessions(context.Background()); len(left) != 0 {
		t.Fatalf("unbounded left: %v", left)
	}
	var revoked bool
	if err = db.DB().QueryRow(`SELECT revoked_at IS NOT NULL FROM principals WHERE session_id='bounded'`).Scan(&revoked); err != nil || revoked {
		t.Fatalf("the bounded credential was revoked (%v, %v)", revoked, err)
	}
}

func TestEnforceFlipProceedsWhenNothingIsUnboundedAndOtherModesNeverCheck(t *testing.T) {
	root, db := guardFixture(t, false)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := flipMode(context.Background(), io.Discard, root, "global", identity.Enforce, "t", "e", false); err != nil {
		t.Fatalf("clean state refused: %v", err)
	}
	// Going the other way never consults the guard, even with unbounded credentials.
	root2, db2 := guardFixture(t, true)
	t.Cleanup(func() { _ = db2.Close() })
	if _, err := flipMode(context.Background(), io.Discard, root2, "global", identity.Observe, "t", "e", false); err != nil {
		t.Fatalf("observe flip refused: %v", err)
	}
	// No state database yet: nothing to revoke, the flip proceeds.
	root3, db3 := guardFixture(t, false)
	_ = db3.Close()
	cat, _ := config.Load(root3)
	_ = os.Remove(config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths))
	if _, err := flipMode(context.Background(), io.Discard, root3, "global", identity.Enforce, "t", "e", false); err != nil {
		t.Fatalf("fresh install refused: %v", err)
	}
}

func TestEnforceGuardFailsClosedWhenTheDatabaseCannotBeRead(t *testing.T) {
	root, db := guardFixture(t, false)
	cat, _ := config.Load(root)
	path := config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths)
	_ = db.Close()
	if err := os.WriteFile(path, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := flipMode(context.Background(), io.Discard, root, "global", identity.Enforce, "t", "e", false); err == nil {
		t.Fatal("an unreadable state database let the flip through")
	}
	if got := fileMode(t, root); got != "observe" {
		t.Fatalf("mode changed to %q", got)
	}
}

// A rollback that would restore enforce is a switch to enforce like any other.
func TestRollbackThatRestoresEnforceIsGuardedToo(t *testing.T) {
	root, db := guardFixture(t, true)
	t.Cleanup(func() { _ = db.Close() })
	// Start from enforce, flip down to observe (recorded, prior = enforce).
	if _, err := applyModeChange(root, "flip", "global", "enforce", "t", "setup"); err != nil {
		t.Fatal(err)
	}
	if _, err := applyModeChange(root, "flip", "global", "observe", "t", "setup"); err != nil {
		t.Fatal(err)
	}
	old := catalogPath
	catalogPath = root
	t.Cleanup(func() { catalogPath = old })
	run := func(args ...string) (string, error) {
		cmd := identityRollbackCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	out, err := run("--point", "global")
	if err == nil || !strings.Contains(err.Error(), "refusing to switch to enforce") {
		t.Fatalf("rollback to enforce not refused: %v\n%s", err, out)
	}
	if got := fileMode(t, root); got != "observe" {
		t.Fatalf("mode %q after the refused rollback", got)
	}
	if _, err = run("--point", "global", "--revoke-anonymous"); err != nil {
		t.Fatal(err)
	}
	if got := fileMode(t, root); got != "enforce" {
		t.Fatalf("mode %q after rollback with --revoke-anonymous", got)
	}
}
