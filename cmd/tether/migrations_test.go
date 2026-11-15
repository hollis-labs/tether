package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

func migrationsStatusOf(t *testing.T, dbPath string) MigrationsStatus {
	t.Helper()
	var out bytes.Buffer
	if err := writeMigrationsStatus(&out, dbPath, true); err != nil {
		t.Fatal(err)
	}
	var st MigrationsStatus
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	return st
}

// CW-20261115-0046: status reports what this binary would migrate, read-only;
// apply brings the database current.
func TestMigrationsStatusAndApply(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	if st := migrationsStatusOf(t, dbPath); st.Exists || len(st.Pending) != 0 {
		t.Fatalf("missing db: %+v", st)
	}
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var latest int
	if err = s.DB().QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`DELETE FROM schema_migrations WHERE version=?`, latest); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if st := migrationsStatusOf(t, dbPath); !st.Exists || !slices.Equal(st.Pending, []int{latest}) || st.InProgress != nil {
		t.Fatalf("one migration behind: %+v (want pending [%d])", st, latest)
	}
	migrationsApplyCmd.SetArgs(nil)
	if err = migrationsApplyCmd.Flags().Set("state-db", dbPath); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	migrationsApplyCmd.SetOut(&out)
	if err = migrationsApplyCmd.RunE(migrationsApplyCmd, nil); err != nil {
		t.Fatalf("apply: %v (%s)", err, out.String())
	}
	if st := migrationsStatusOf(t, dbPath); len(st.Pending) != 0 {
		t.Fatalf("after apply: %+v", st)
	}
}

// Host-only: migrations touch the local state database.
func TestMigrationsRefusedWithARemoteTarget(t *testing.T) {
	remoteTargetFixture(t, "tcp:127.0.0.1:8099")
	for _, path := range [][]string{{"migrations", "status"}, {"migrations", "apply"}} {
		cmd, _, err := rootCmd.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := guardRemoteCommand(cmd, nil); err == nil {
			t.Fatalf("%v admitted with --target", path)
		}
	}
}
