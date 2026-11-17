package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
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
	// Construct a real pre-migration database. Deleting a ledger row from a
	// current database leaves the migration's tables and columns in place.
	entries, err := os.ReadDir("../../internal/store/migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected multiple migrations, got %d", len(entries))
	}
	latest, err := strconv.Atoi(strings.SplitN(entries[len(entries)-1].Name(), "_", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries[:len(entries)-1] {
		name := strings.TrimSuffix(entry.Name(), ".sql")
		parts := strings.SplitN(name, "_", 2)
		version, err := strconv.Atoi(parts[0])
		if err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join("../../internal/store/migrations", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(string(body)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply fixture migration %s: %v", name, err)
		}
		if _, err = tx.Exec(`INSERT INTO schema_migrations(version,name,applied_at) VALUES(?,?,?)`, version, parts[1], "test"); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
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
