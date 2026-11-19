package migrate

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/core/store"
	libmigrate "github.com/hollis-labs/libs/core/store/migrate"
	_ "modernc.org/sqlite" // the driver the database module uses

	"github.com/hollis-labs/tether/next/migrations"
)

var update = flag.Bool("update", false, "regenerate migrations/sqlite/00001_schema.sql from the old migrations")

// schemaFile is the squashed migration, relative to this package.
const schemaFile = "../../migrations/sqlite/00001_schema.sql"

// excluded names every object the old migrations create that the squashed schema deliberately does
// not, with the reason. Nothing else may differ.
var excluded = map[string]string{
	// Port plan D3: the LLM gateway is out of scope. Its rows are archived to llm-archive.db by
	// migrate-from (S12), never loaded into the new database.
	"ai_events":                       "D3: LLM gateway audit, archived not migrated",
	"idx_ai_events_timestamp":         "D3: index of ai_events",
	"idx_ai_events_provider":          "D3: index of ai_events",
	"idx_ai_events_session":           "D3: index of ai_events",
	"idx_ai_events_operation_purpose": "D3: index of ai_events",
	"schema_migrations":               "the old migrator's ledger; goose_db_version replaces it (kept in the archive as provenance)",
}

// newOnly names objects only the new database has, with the reason.
var newOnly = map[string]string{
	"goose_db_version":     "the database module's goose version table",
	"idx_messages_seq":     "00002: messages.seq is unique (plan 13.1)",
	"messages_assign_seq":  "00002: SQLite assigns messages.seq to a new row (Postgres uses an identity column)",
	"replaced_shim_delete": "00006: the old store created this guard in Go at open (session_replacement.go); here it is a migration",
}

// transform is a table whose new shape differs from the old one by design: exactly the listed columns
// are added (by a later migration, so they are last), together with their indexes. migrate-from fills
// them; everything else about the table must still equal the old schema.
type transform struct {
	why     string
	columns map[string]string // added column -> the text ALTER TABLE ADD COLUMN appends to the table's SQL
	indexes []string
}

var transforms = map[string]transform{
	"messages": {
		why: "plan 13.1: an explicit seq holding exactly the old rowid replaces the implicit rowid as the " +
			"mailbox watch cursor (seq = old rowid, verified by VerifyMessagesSeq)",
		columns: map[string]string{"seq": ", seq INTEGER NOT NULL DEFAULT 0"},
		indexes: []string{"idx_messages_seq"},
	},
}

// withoutTransform removes a transform table's added columns and indexes from its new description, so
// what remains must equal the old one.
func withoutTransform(desc []string, tr transform) []string {
	var out []string
outer:
	for _, l := range desc {
		for col, sqlText := range tr.columns {
			if strings.HasPrefix(l, "sql=") {
				l = strings.Replace(l, normSQL(sql.NullString{String: sqlText, Valid: true}), "", 1)
			}
			if strings.HasPrefix(l, "column ") && strings.Contains(l, " name="+col+" ") {
				continue outer
			}
		}
		for _, idx := range tr.indexes {
			if strings.HasPrefix(l, "index ") && (strings.Contains(l, " name="+idx+" ") || strings.HasPrefix(l, "index "+idx+" key ")) {
				continue outer
			}
		}
		out = append(out, l)
	}
	return out
}

// Not covered by this test, by design (each is recorded in docs/architecture.md):
//   - messaging_*: the seven tables go-messaging's delivery.ApplySQLiteSchema creates at open. They
//     belong to the library; neither the old migrations nor 00001 create them (TestNoLibraryTables).
//   - the three triggers internal/store/session_replacement.go installs in Go at open: recreated by
//     the ported store code (S2), so neither side here has them.
//   - leftover *_new / *_v2 tables: every one the old migrations create is renamed or dropped by a
//     later migration (TestNoLeftovers), and the live inventory holds none.

// oldMigration is one vendored file of the old tree's internal/store/migrations at f6c37bfa.
type oldMigration struct {
	version int
	name    string
	sql     string
}

var oldFileRe = regexp.MustCompile(`^(\d+)_(.+)\.sql$`)

func readOld(t *testing.T) []oldMigration {
	t.Helper()
	ents, err := os.ReadDir("testdata/oldmigrations")
	if err != nil {
		t.Fatal(err)
	}
	var out []oldMigration
	for _, e := range ents {
		m := oldFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			t.Fatalf("unexpected file %s in testdata/oldmigrations", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		b, err := os.ReadFile(filepath.Join("testdata/oldmigrations", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, oldMigration{version: v, name: m[2], sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			t.Fatalf("old migrations: version %d at position %d, want a gapless 1..N", m.version, i+1)
		}
	}
	return out
}

// squashedAt is the old migration 00001_schema.sql squashes (main f6c37bfa). The vendored files after it
// are carried from main as 00003 onwards, one migration per dialect (carry ledger).
const squashedAt = 101

// buildOld applies every vendored migration the way the old migrator does: its ledger table first,
// then each file as one multi-statement Exec inside its own transaction, in version order.
func buildOld(t *testing.T) *sql.DB { return buildOldTo(t, 0) }

// buildOldTo is buildOld up to and including version upTo (0 for all).
func buildOldTo(t *testing.T, upTo int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
        version    INTEGER PRIMARY KEY,
        name       TEXT NOT NULL,
        applied_at TEXT NOT NULL
    )`); err != nil {
		t.Fatal(err)
	}
	for _, m := range readOld(t) {
		if upTo > 0 && m.version > upTo {
			break
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(m.sql); err != nil {
			t.Fatalf("old migration %04d_%s: %v", m.version, m.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations VALUES (?, ?, '2026-11-14T00:00:00Z')`, m.version, m.name); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// buildNew applies the embedded migration set through the same go-store/goose path the database
// module runs at start.
func buildNew(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "new.db")
	st, err := store.Open(ctx, store.Config{Driver: store.DriverSQLite, SQLite: store.SQLiteConfig{Path: p}})
	if err != nil {
		t.Fatal(err)
	}
	fsys, root := migrations.FS()
	if err := libmigrate.UpStore(ctx, st, libmigrate.Source{FS: fsys, Root: root}); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type object struct {
	typ, name, table string
	sql              sql.NullString
}

func objects(t *testing.T, db *sql.DB) []object {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []object
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.typ, &o.name, &o.table, &o.sql); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

var spaceRe = regexp.MustCompile(`\s+`)

func normSQL(s sql.NullString) string {
	return strings.TrimSpace(spaceRe.ReplaceAllString(s.String, " "))
}

// pragmaRows renders every row of a PRAGMA call as text, so two databases compare line by line.
func pragmaRows(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			parts[i] = fmt.Sprintf("%s=%v", cols[i], v)
		}
		out = append(out, strings.Join(parts, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// describe is the normalized form compared per object: its SQL text (whitespace collapsed) and, for
// a table, its columns (type, NOT NULL, default, key position, hidden), every index with its columns,
// uniqueness, origin and partial flag, and its foreign keys.
func describe(t *testing.T, db *sql.DB, o object) []string {
	t.Helper()
	out := []string{"type=" + o.typ, "table=" + o.table, "sql=" + normSQL(o.sql)}
	if o.typ != "table" {
		return out
	}
	q := quoteIdent(o.name)
	out = append(out, prefix("column ", pragmaRows(t, db, "PRAGMA table_xinfo("+q+")"))...)
	for _, idx := range pragmaRows(t, db, "PRAGMA index_list("+q+")") {
		// seq is position-dependent and not part of the shape.
		out = append(out, "index "+regexp.MustCompile(`^seq=\S+ `).ReplaceAllString(idx, ""))
		name := strings.Fields(strings.SplitN(idx, "name=", 2)[1])[0]
		out = append(out, prefix("index "+name+" key ", pragmaRows(t, db, "PRAGMA index_xinfo("+quoteIdent(name)+")"))...)
	}
	out = append(out, prefix("fk ", pragmaRows(t, db, "PRAGMA foreign_key_list("+q+")"))...)
	return out
}

func prefix(p string, xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = p + x
	}
	return out
}

// dataRows returns every row of a table, rendered, for the seed-data comparison.
func dataRows(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	cols := columnNames(t, db, table)
	order := make([]string, len(cols))
	for i := range cols {
		order[i] = strconv.Itoa(i + 1)
	}
	return pragmaRows(t, db, "SELECT * FROM "+quoteIdent(table)+" ORDER BY "+strings.Join(order, ", "))
}

// generated is true for objects SQLite creates itself (autoindexes, sqlite_sequence, FTS5 shadow
// tables), which a migration must not create.
func generated(o object, all []object) bool {
	if !o.sql.Valid || strings.HasPrefix(o.name, "sqlite_") {
		return true
	}
	for _, v := range all {
		if v.typ == "table" && strings.HasPrefix(strings.ToUpper(normSQL(v.sql)), "CREATE VIRTUAL TABLE") &&
			o.typ == "table" && strings.HasPrefix(o.name, v.name+"_") {
			return true
		}
	}
	return false
}

func TestSchemaEqualsOld(t *testing.T) {
	old := buildOld(t)
	if *update {
		writeSchema(t, buildOldTo(t, squashedAt))
	}
	nw := buildNew(t)
	oldObjs, newObjs := objects(t, old), objects(t, nw)

	byName := func(xs []object) map[string]object {
		m := map[string]object{}
		for _, o := range xs {
			m[o.name] = o
		}
		return m
	}
	om, nm := byName(oldObjs), byName(newObjs)

	for name, why := range excluded {
		if _, ok := om[name]; !ok {
			t.Errorf("allowlist names %s (%s), but the old migrations do not create it: remove the entry", name, why)
		}
		if _, ok := nm[name]; ok {
			t.Errorf("%s is excluded (%s) but the new schema creates it", name, why)
		}
	}
	for name, tr := range transforms {
		if _, ok := om[name]; !ok {
			t.Errorf("transform names %s (%s), but the old migrations do not create it", name, tr.why)
		}
		for _, idx := range tr.indexes {
			if _, ok := newOnly[idx]; !ok {
				t.Errorf("transform %s adds index %s, which must also be on the new-only allowlist", name, idx)
			}
		}
	}
	for name, why := range newOnly {
		if _, ok := nm[name]; !ok {
			t.Errorf("new-only %s (%s) is missing from the new database", name, why)
		}
	}
	for _, o := range oldObjs {
		if _, skip := excluded[o.name]; skip {
			continue
		}
		n, ok := nm[o.name]
		if !ok {
			t.Errorf("old %s %s is not in the new schema and not on the allowlist", o.typ, o.name)
			continue
		}
		a, b := describe(t, old, o), describe(t, nw, n)
		if tr, ok := transforms[o.name]; ok {
			b = withoutTransform(b, tr)
		}
		if strings.Join(a, "\n") != strings.Join(b, "\n") {
			t.Errorf("%s %s differs:\nold:\n  %s\nnew:\n  %s", o.typ, o.name, strings.Join(a, "\n  "), strings.Join(b, "\n  "))
		}
	}
	for _, n := range newObjs {
		if _, ok := om[n.name]; !ok {
			if _, allowed := newOnly[n.name]; !allowed && !strings.HasPrefix(n.name, "sqlite_autoindex_goose") {
				t.Errorf("new %s %s is not in the old schema and not on the new-only allowlist", n.typ, n.name)
			}
		}
	}

	// A fresh database must also hold the same seed rows (migration 0091 seeds
	// message_event_checkpoints), or a fresh install would start in a different state.
	for _, o := range oldObjs {
		if o.typ != "table" || generated(o, oldObjs) {
			continue
		}
		if _, skip := excluded[o.name]; skip {
			continue
		}
		a, b := dataRows(t, old, o.name), dataRows(t, nw, o.name)
		if strings.Join(a, "\n") != strings.Join(b, "\n") {
			t.Errorf("seed rows of %s differ:\nold: %v\nnew: %v", o.name, a, b)
		}
	}
}

// TestNoLibraryTables keeps go-messaging's tables out of the squashed schema: the library creates
// and evolves them (delivery.ApplySQLiteSchema at open).
func TestNoLibraryTables(t *testing.T) {
	for _, o := range objects(t, buildNew(t)) {
		if strings.HasPrefix(o.name, "messaging_") {
			t.Errorf("%s %s is go-messaging's; the library creates it, not this schema", o.typ, o.name)
		}
	}
}

// TestNoLeftovers proves no transitional *_new / *_v2 / *_verified / *_upgrade table survives the
// old migrations, so the squashed schema drops none with rows in it.
func TestNoLeftovers(t *testing.T) {
	re := regexp.MustCompile(`_(new|v2|verified|upgrade)$`)
	for _, o := range objects(t, buildOld(t)) {
		if o.typ == "table" && re.MatchString(o.name) {
			t.Errorf("old migrations leave transitional table %s", o.name)
		}
	}
}

// writeSchema renders 00001_schema.sql from the old database: every object in creation order, minus
// the allowlist and what SQLite creates itself, then the seed rows.
func writeSchema(t *testing.T, old *sql.DB) {
	t.Helper()
	objs := objects(t, old)
	var b strings.Builder
	b.WriteString(`-- Tether's squashed schema: the old store's schema at its migration 0101 (main f6c37bfa,
-- internal/store/migrations), minus the objects the allowlist in internal/migrate/schema_test.go names.
-- GENERATED: do not edit. Regenerate with
--   go test ./internal/migrate -run TestSchemaEqualsOld -update
-- and review the diff table by table. Later schema changes are new migrations (00002 onwards), never
-- edits here. One-way: there is no Down.
-- The seven messaging_* tables are go-messaging's and are created by the library at open.

-- +goose Up
`)
	for _, o := range objs {
		if _, skip := excluded[o.name]; skip || generated(o, objs) {
			continue
		}
		s := strings.TrimSpace(o.sql.String)
		if o.typ == "trigger" {
			fmt.Fprintf(&b, "\n-- +goose StatementBegin\n%s;\n-- +goose StatementEnd\n", s)
			continue
		}
		fmt.Fprintf(&b, "\n%s;\n", s)
	}
	seeded := false
	for _, o := range objs {
		if o.typ != "table" || generated(o, objs) {
			continue
		}
		if _, skip := excluded[o.name]; skip {
			continue
		}
		cols := columnNames(t, old, o.name)
		for _, vals := range quotedRows(t, old, o.name, cols) {
			if !seeded {
				b.WriteString("\n-- Seed rows a fresh old database holds.\n")
				seeded = true
			}
			fmt.Fprintf(&b, "INSERT INTO %s (%s) VALUES (%s);\n", o.name, strings.Join(cols, ", "), strings.Join(vals, ", "))
		}
	}
	if err := os.WriteFile(schemaFile, []byte(b.String()), 0o644); err != nil { //nolint:gosec // a source file, world-readable like the rest of the tree
		t.Fatal(err)
	}
	t.Logf("wrote %s", schemaFile)
}

func columnNames(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query("SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// quotedRows returns each row of a table as SQL literals (SQLite's quote()), in key order.
func quotedRows(t *testing.T, db *sql.DB, table string, cols []string) [][]string {
	t.Helper()
	exprs := make([]string, len(cols))
	for i, c := range cols {
		exprs[i] = "quote(" + quoteIdent(c) + ")"
	}
	rows, err := db.Query("SELECT " + strings.Join(exprs, ", ") + " FROM " + quoteIdent(table) + " ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out [][]string
	for rows.Next() {
		vals := make([]string, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
