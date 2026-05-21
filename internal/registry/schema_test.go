package registry_test

import (
	"database/sql"
	"sort"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hollis-labs/tether/internal/store"
)

// TestMigration0015_tablesAndIndexes verifies the registry tables and
// indexes are created by migration 0015 against a fresh DB.
func TestMigration0015_tablesAndIndexes(t *testing.T) {
	db := openInMemory(t)
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	gotTables := loadObjects(t, db, "table", "registry_%")
	wantTables := []string{
		"registry_capabilities",
		"registry_entries",
		"registry_links",
		"registry_skills",
	}
	if !sliceEqual(gotTables, wantTables) {
		t.Fatalf("tables = %v; want %v", gotTables, wantTables)
	}

	gotIndexes := loadObjects(t, db, "index", "idx_registry_%")
	wantIndexes := []string{
		"idx_registry_capabilities_capability",
		"idx_registry_entries_kind",
		"idx_registry_entries_kind_project",
		"idx_registry_entries_kind_role",
		"idx_registry_entries_kind_status",
		"idx_registry_skills_name",
	}
	if !sliceEqual(gotIndexes, wantIndexes) {
		t.Fatalf("indexes = %v; want %v", gotIndexes, wantIndexes)
	}
}

// TestMigration0015_idempotent verifies re-running Migrate on an
// already-migrated DB is a no-op (no rows in Applied).
func TestMigration0015_idempotent(t *testing.T) {
	db := openInMemory(t)
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate (first): %v", err)
	}
	res, err := store.Migrate(db)
	if err != nil {
		t.Fatalf("migrate (second): %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("second migrate applied = %v; want empty", res.Applied)
	}
}

// TestMigration0015_kindCheckConstraint verifies the kind CHECK rejects
// values outside the {'agent','project'} set.
func TestMigration0015_kindCheckConstraint(t *testing.T) {
	db := openInMemory(t)
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, err := db.Exec(`INSERT INTO registry_entries
		(urn, kind, display_name, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		"msg://agent/agent-mux/agt_test123456", "unknown", "Test", "active",
		"2026-05-20T00:00:00Z", "2026-05-20T00:00:00Z")
	if err == nil {
		t.Fatal("insert with kind='unknown' succeeded; want CHECK violation")
	}
}

// TestMigration0015_defaultsAndNulls confirms the schema's defaults +
// nullable columns behave as declared.
func TestMigration0015_defaultsAndNulls(t *testing.T) {
	db := openInMemory(t)
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO registry_entries
		(urn, kind, display_name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`,
		"msg://agent/agent-mux/agt_test123456", "agent", "Test",
		"2026-05-20T00:00:00Z", "2026-05-20T00:00:00Z"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var (
		muxInstance string
		status      string
	)
	if err := db.QueryRow(`SELECT mux_instance_id, status FROM registry_entries
		WHERE urn = ?`, "msg://agent/agent-mux/agt_test123456").Scan(&muxInstance, &status); err != nil {
		t.Fatalf("select: %v", err)
	}
	if muxInstance != "agent-mux" {
		t.Fatalf("mux_instance_id default = %q; want %q", muxInstance, "agent-mux")
	}
	if status != "active" {
		t.Fatalf("status default = %q; want %q", status, "active")
	}
}

func openInMemory(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func loadObjects(t *testing.T, db *sql.DB, objType, like string) []string {
	t.Helper()
	rows, err := db.Query(
		`SELECT name FROM sqlite_master WHERE type=? AND name LIKE ? ORDER BY name`,
		objType, like,
	)
	if err != nil {
		t.Fatalf("query %s: %v", objType, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]string{}, a...)
	bb := append([]string{}, b...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
