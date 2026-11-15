package store_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

// libraryTables are go-messaging's: classed (C) but created by the library at open, not by migrations.
var libraryTables = map[string]bool{
	"messaging_attempts": true, "messaging_deliveries": true, "messaging_delivery_schema": true,
	"messaging_idempotency": true, "messaging_messages": true, "messaging_receipts": true,
	"messaging_transitions": true,
}

func sqliteObjects(t *testing.T, typ string) map[string][2]string {
	t.Helper()
	st := storetest.Raw(t, gostore.DriverSQLite)
	rows, err := st.Reader().QueryContext(context.Background(),
		`SELECT name, tbl_name, COALESCE(sql, '') FROM sqlite_master WHERE type = ?`, typ)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][2]string{}
	for rows.Next() {
		var name, table, sql string
		if err := rows.Scan(&name, &table, &sql); err != nil {
			t.Fatal(err)
		}
		out[name] = [2]string{table, sql}
	}
	return out
}

// engineTables are migrate-from's bookkeeping: classed (A) but created by the engine, not by migrations.
var engineTables = map[string]bool{
	"tether_migration_mark": true, "tether_migration_tables": true, "tether_migration_triggers": true,
	"tether_migration_ranges": true,
}

// TestEveryTableHasAClass: each table the migrations create has exactly one class, and the class map
// names no table that does not exist.
func TestEveryTableHasAClass(t *testing.T) {
	tables := sqliteObjects(t, "table")
	for name := range tables {
		if name == "goose_db_version" || strings.HasPrefix(name, "sqlite_") {
			continue
		}
		if strings.HasPrefix(name, "forum_post_fts_") {
			continue // FTS5's shadow tables, created by SQLite for forum_post_fts
		}
		if _, ok := store.ClassOf(name); !ok {
			t.Errorf("table %s has no class: add it to classmap.go with its reason", name)
		}
	}
	for _, name := range store.ClassedTables() {
		if _, ok := tables[name]; !ok && !libraryTables[name] && !engineTables[name] {
			t.Errorf("classmap.go names %s, which no migration creates", name)
		}
	}
}

// triggerCrossings names every trigger whose body reaches a table of another class than the table it
// fires on, with the slice that redesigns it. A trigger cannot reach across databases, so each must be
// gone or moved before either class moves (plan section 13.7). Nothing else may cross.
var triggerCrossings = map[string]string{
	// B-5: session state (C) revokes session credentials (A).
	"session_terminal_revokes_credentials": "S9 (B-5): writes principals (A) from sessions (C)",
	"session_delete_revokes_credentials":   "S9 (B-5): writes principals (A) from sessions (C)",
	// Auth (A) checks that read session and team state (C).
	"session_principal_requires_active_session": "S3/S9 (B-5): principals (A) reads sessions (C)",
	"replacement_principal_authority":           "S3/S9 (B-5): principals (A) reads replacement, binding, team and registry state (C)",
	// B-1: events (B) checks the event identity (A).
	"events_require_id_after_seed": "S8 (B-1): events (B) reads event_identity (A)",
}

var tableRefRe = regexp.MustCompile(`(?i)\b(?:FROM|JOIN|INTO|UPDATE)\s+"?([A-Za-z_][A-Za-z0-9_]*)`)

// TestTriggerCrossings finds the classes each trigger's body reaches and requires every crossing to be on
// the triggerCrossings list, and every entry on the list to still cross.
func TestTriggerCrossings(t *testing.T) {
	var found []string
	for name, v := range sqliteObjects(t, "trigger") {
		own, ok := store.ClassOf(v[0])
		if !ok {
			t.Fatalf("trigger %s fires on %s, which has no class", name, v[0])
		}
		var other []string
		for _, m := range tableRefRe.FindAllStringSubmatch(v[1], -1) {
			if c, ok := store.ClassOf(strings.ToLower(m[1])); ok && c != own {
				other = append(other, m[1]+" ("+c.String()+")")
			}
		}
		if len(other) == 0 {
			if why, listed := triggerCrossings[name]; listed {
				t.Errorf("trigger %s no longer crosses classes; remove it from triggerCrossings (%s)", name, why)
			}
			continue
		}
		found = append(found, name)
		if _, listed := triggerCrossings[name]; !listed {
			t.Errorf("trigger %s on %s (%s) reaches %s: add it to triggerCrossings with its slice", name, v[0], own, strings.Join(other, ", "))
		}
	}
	sort.Strings(found)
	t.Logf("cross-class triggers: %v", found)
	for name := range triggerCrossings {
		if !contains(found, name) {
			t.Errorf("triggerCrossings names %s, which does not cross classes (or no longer exists)", name)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// rawDBAllowed names the functions that may call .DB(), with the reason.
var rawDBAllowed = map[string]string{
	"LibraryDB": "the sanctioned handle for go-messaging's tables (the library takes a raw *sql.DB)",
}

// TestNoRawDB keeps every query on go-store's rebinding Querier: no non-test code in next/ calls .DB()
// (the raw *sql.DB, which would take "?" placeholders to Postgres unbound and bypass the class checks),
// except the functions on rawDBAllowed.
func TestNoRawDB(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || rawDBAllowed[fn.Name.Name] == "" {
				continue
			}
			fn.Body = nil // allowed: skip its body
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 0 {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "DB" {
				t.Errorf("%s: .DB() bypasses the rebinding Querier and the class checks", fset.Position(call.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
