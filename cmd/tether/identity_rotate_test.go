package main

import (
	"bytes"
	"context"
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
	if err := runRotateOperator(ctx, &out, root, dbPath, tokenPath, "tester", "CW-20261002-0003 test"); err != nil {
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
	if err := runRotateOperator(ctx, &bytes.Buffer{}, root, dbPath, tokenPath, "tester", "x"); err == nil {
		t.Fatal("rotated with a broken audit log")
	}
	db, _ = store.Open(dbPath)
	defer func() { _ = db.Close() }()
	if _, err := identity.NewStore(db.DB()).Verify(ctx, old); err != nil {
		t.Fatal("old credential revoked by a refused rotation")
	}
}
