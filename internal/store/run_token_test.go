package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRunTokenDenylistSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"jti-denied", "run-denied"} {
		if err := db.RevokeRunToken(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct {
		jti, run string
		want     bool
	}{{"jti-denied", "other", true}, {"other", "run-denied", true}, {"other", "other", false}} {
		got, err := db.RunTokenDenied(ctx, tc.jti, tc.run)
		if err != nil || got != tc.want {
			t.Fatal("revocation mismatch", err)
		}
	}
	if err := db.RevokeRunToken(ctx, "run-denied"); err != nil {
		t.Fatal("idempotent revoke", err)
	}
	for _, id := range []string{"", "not.a.token", "line\nbreak", " "} {
		if err := db.RevokeRunToken(ctx, id); err == nil {
			t.Fatal("invalid identifier accepted")
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RunTokenDenied(ctx, "jti", "run"); err == nil {
		t.Fatal("closed store failed open")
	}
}
