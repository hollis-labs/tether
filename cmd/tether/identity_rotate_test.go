package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func TestRotateOperatorCommandRotatesAuditsAndPrintsNoSecret(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	state := t.TempDir()
	root := filepath.Join(state, "catalog")
	dbPath := filepath.Join(state, "state", "tether.db")
	tokenPath := filepath.Join(state, "run", "operator.token")
	ctx := context.Background()

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ids := identity.NewStore(db.DB())
	if err := ids.EnsureOperator(ctx, tokenPath); err != nil {
		t.Fatal(err)
	}
	old, err := identity.ReadTokenFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	var out bytes.Buffer
	if err := runRotateOperator(ctx, &out, root, dbPath, tokenPath, "tester", "CW-20261002-0003 test", nil); err != nil {
		t.Fatal(err)
	}

	db, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ids = identity.NewStore(db.DB())
	if _, err := ids.Verify(ctx, old); err == nil {
		t.Fatal("old operator token still valid")
	}
	fresh, err := identity.ReadTokenFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := ids.Verify(ctx, fresh); err != nil || p.ID != identity.OperatorID {
		t.Fatalf("new token invalid: %v", err)
	}
	for _, secret := range []string{old, fresh} {
		if strings.Contains(out.String(), secret) {
			t.Fatal("command output contains a credential")
		}
	}
	logPath := flipLogPath(root)
	recs, _, err := verifyFlipLog(logPath)
	if err != nil || len(recs) != 1 || recs[0].Action != "rotate-operator" || recs[0].Point != "operator" || recs[0].Evidence == "" {
		t.Fatalf("audit record: %v %+v", err, recs)
	}
	raw, _ := os.ReadFile(logPath)
	for _, secret := range []string{old, fresh} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("audit log contains a credential")
		}
	}
	if len(pendingFlips(recs)) != 0 {
		t.Fatal("a rotation must not count as a rollback-able mode flip")
	}
}

func TestRotateOperatorCommandRefusesBrokenAuditLogBeforeChanging(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	state := t.TempDir()
	root := filepath.Join(state, "catalog")
	dbPath := filepath.Join(state, "state", "tether.db")
	tokenPath := filepath.Join(state, "run", "operator.token")
	ctx := context.Background()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ids := identity.NewStore(db.DB())
	if err := ids.EnsureOperator(ctx, tokenPath); err != nil {
		t.Fatal(err)
	}
	old, _ := identity.ReadTokenFile(tokenPath)
	_ = db.Close()
	if err := os.MkdirAll(filepath.Dir(flipLogPath(root)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flipLogPath(root), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runRotateOperator(ctx, &bytes.Buffer{}, root, dbPath, tokenPath, "tester", "x", nil); err == nil {
		t.Fatal("rotated with a broken audit log")
	}
	db, _ = store.Open(dbPath)
	defer func() { _ = db.Close() }()
	if _, err := identity.NewStore(db.DB()).Verify(ctx, old); err != nil {
		t.Fatal("old credential revoked by a refused rotation")
	}
}

// After rotating, the command asks the running daemon to deny what the old
// credential had issued, authenticating with the new token file, and reports
// the outcome; a failure there is a warning, not a failed rotation.
func TestRotateOperatorDeniesIssuedTokensAndWarnsWhenItCannot(t *testing.T) {
	for name, tc := range map[string]struct {
		deny    func(context.Context, string) (int, error)
		wantOut string
	}{
		"denied": {func(_ context.Context, tokenPath string) (int, error) {
			if _, err := identity.ReadTokenFile(tokenPath); err != nil {
				t.Errorf("the new token file was not in place when the daemon was asked: %v", err)
			}
			return 4, nil
		}, "denied 4 issued token(s)"},
		"daemon-down": {func(context.Context, string) (int, error) { return 0, errors.New("daemon not reachable") }, "WARNING: could not deny tokens already issued"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			state := t.TempDir()
			root := filepath.Join(state, "catalog")
			dbPath := filepath.Join(state, "state", "tether.db")
			tokenPath := filepath.Join(state, "run", "operator.token")
			db, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := identity.NewStore(db.DB()).EnsureOperator(context.Background(), tokenPath); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			var out bytes.Buffer
			if err := runRotateOperator(context.Background(), &out, root, dbPath, tokenPath, "tester", "test", tc.deny); err != nil {
				t.Fatalf("rotation reported failure: %v", err)
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("output %q lacks %q", out.String(), tc.wantOut)
			}
		})
	}
}
