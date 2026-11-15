package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/core/store"
	libmigrate "github.com/hollis-labs/libs/core/store/migrate"
	"github.com/hollis-labs/libs/core/store/storetest"

	_ "github.com/hollis-labs/tether/next/internal/sqlitefk" // foreign keys off, as the binary runs
	"github.com/hollis-labs/tether/next/migrations"
)

// envPostgresDSN names the Postgres server the Postgres leg runs on (port plan section 13.1(3)). Each test
// gets a private schema on it, dropped at the end, so the server can be shared.
const envPostgresDSN = "SUBSTRATE_TEST_POSTGRES_DSN"

const pgSkipNotice = "NOTICE: " + envPostgresDSN + " is unset, so the Postgres leg of internal/migrate did not run " +
	"(port plan 13.1(3): it becomes a required check before S2 merges)"

func TestMain(m *testing.M) {
	if os.Getenv(envPostgresDSN) == "" {
		// Visible without -v, so a skipped Postgres leg is never mistaken for a passing one.
		fmt.Fprintln(os.Stderr, pgSkipNotice)
	}
	code := m.Run()
	if fixtureDir != "" {
		_ = os.RemoveAll(fixtureDir)
	}
	os.Exit(code)
}

func migrationSource() libmigrate.Source {
	fsys, root := migrations.FS()
	return libmigrate.Source{FS: fsys, Root: root}
}

// migrated applies the embedded set for the store's dialect twice: the second run must find nothing to
// do and leave the version at the latest one.
func migrated(t *testing.T, s *store.Store) *store.Store {
	t.Helper()
	ctx := context.Background()
	src := migrationSource()
	for i := 0; i < 2; i++ {
		if err := libmigrate.UpStore(ctx, s, src); err != nil {
			t.Fatalf("%s migrate (run %d): %v", s.Dialect().Name(), i+1, err)
		}
	}
	want, err := src.Latest(s.Dialect().Name())
	if err != nil {
		t.Fatal(err)
	}
	got, err := libmigrate.Current(ctx, s.Reader(), src)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s at version %d after two runs, want %d", s.Dialect().Name(), got, want)
	}
	return s
}

// sqliteStore is a migrated SQLite store in the test's temp directory.
func sqliteStore(t *testing.T) *store.Store {
	t.Helper()
	return migrated(t, storetest.SQLite(t))
}

// pgStore is a migrated Postgres store on a private schema, or it skips the test with the notice.
func pgStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv(envPostgresDSN)
	if dsn == "" {
		t.Skip(pgSkipNotice)
	}
	t.Setenv(storetest.EnvDSN, dsn)
	return migrated(t, storetest.Postgres(t))
}

// eachDialect runs fn on a migrated SQLite store and a migrated Postgres store.
func eachDialect(t *testing.T, fn func(t *testing.T, s *store.Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { fn(t, sqliteStore(t)) })
	t.Run("postgres", func(t *testing.T) { fn(t, pgStore(t)) })
}

func TestMigrationsIdempotent(t *testing.T) {
	eachDialect(t, func(t *testing.T, s *store.Store) {})
}

// pgTypes maps a SQLite declared type to the Postgres type the schema rules give it.
var pgTypes = map[string]string{
	"INTEGER":   "bigint",
	"TEXT":      "text",
	"DATETIME":  "text",
	"TIMESTAMP": "text",
	"REAL":      "double precision",
	"BLOB":      "bytea",
}

// pgOnly names Postgres objects the SQLite set does not have, with the reason.
var pgOnly = map[string]string{
	"forum_post_fts_search":        "the GIN index over forum_post_fts.search, Postgres's form of the FTS5 index",
	"message_aliases_alias_nocase": "UNIQUE COLLATE NOCASE on message_aliases.alias, as a unique index on lower(alias)",
}

// pgOnlyTriggers names Postgres triggers the SQLite set does not have, with the reason.
var pgOnlyTriggers = map[string]string{
	"message_event_journal_order": "00005: journal ids commit in order (an advisory lock; SQLite has one writer)",
}

// sqliteOnly names SQLite objects (indexes, triggers) the Postgres set does not have, with the reason.
var sqliteOnly = map[string]string{
	"messages_assign_seq": "Postgres assigns messages.seq from an identity column",
}

// columnExceptions names columns whose default or identity differ by design.
var columnExceptions = map[string]string{
	"messages.seq": "SQLite: NOT NULL DEFAULT 0, set by messages_assign_seq; Postgres: an identity column",
}

// uniqueAliases maps a SQLite unique key to the Postgres one that enforces it differently.
var uniqueAliases = map[string]string{
	"message_aliases(alias)": "message_aliases(<expr>)",
}

// ftsTable is the forum search index: an FTS5 virtual table on SQLite, a table with a generated tsvector
// on Postgres. Only its visible columns are compared.
const ftsTable = "forum_post_fts"

type pgColumn struct {
	name, typ, collation string
	notNull, identity    bool
	dflt                 sql.NullString
}

func pgColumns(t *testing.T, db *sql.DB, table string) []pgColumn {
	t.Helper()
	rows, err := db.Query(`SELECT column_name, data_type, COALESCE(collation_name, ''), is_nullable = 'NO',
		is_identity = 'YES', column_default
		FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1
		ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []pgColumn
	for rows.Next() {
		var c pgColumn
		if err := rows.Scan(&c.name, &c.typ, &c.collation, &c.notNull, &c.identity, &c.dflt); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func strs(t *testing.T, db *sql.DB, q string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// pgIndexes describes each index of a table: name -> "unique=.. partial=.. cols=..", and whether a
// constraint (primary key or UNIQUE) backs it.
func pgIndexes(t *testing.T, db *sql.DB, table string) (named map[string]string, keys []string) {
	t.Helper()
	rows, err := db.Query(`SELECT ic.relname, i.indisunique, i.indpred IS NOT NULL,
		EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = i.indexrelid),
		(SELECT string_agg(CASE WHEN u.k = 0 THEN '<expr>' ELSE a.attname END, ',' ORDER BY u.n)
		   FROM unnest(i.indkey::int2[]) WITH ORDINALITY AS u(k, n)
		   LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = u.k)
		FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
		WHERE i.indrelid = $1::regclass`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	named = map[string]string{}
	for rows.Next() {
		var name, cols string
		var unique, partial, backed bool
		if err := rows.Scan(&name, &unique, &partial, &backed, &cols); err != nil {
			t.Fatal(err)
		}
		if unique {
			keys = append(keys, fmt.Sprintf("%s(%s) partial=%v", table, cols, partial))
		}
		if !backed {
			named[name] = fmt.Sprintf("unique=%v partial=%v cols=%s", unique, partial, cols)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	return named, keys
}

// sqliteIndexes is pgIndexes for SQLite: indexes from CREATE INDEX by name, and every unique key.
func sqliteIndexes(t *testing.T, db *sql.DB, table string) (named map[string]string, keys []string) {
	t.Helper()
	named = map[string]string{}
	q := quoteIdent(table)
	for _, l := range pragmaRows(t, db, "PRAGMA index_list("+q+")") {
		f := map[string]string{}
		for _, kv := range strings.Fields(l) {
			if k, v, ok := strings.Cut(kv, "="); ok {
				f[k] = v
			}
		}
		var cols []string
		rows, err := db.Query("SELECT cid, COALESCE(name, '') FROM pragma_index_xinfo(?) WHERE key = 1 ORDER BY seqno", f["name"])
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var cid int
			var name string
			if err := rows.Scan(&cid, &name); err != nil {
				t.Fatal(err)
			}
			if cid == -2 {
				name = "<expr>"
			}
			cols = append(cols, name)
		}
		_ = rows.Close()
		unique, partial := f["unique"] == "1", f["partial"] == "1"
		if unique {
			keys = append(keys, fmt.Sprintf("%s(%s) partial=%v", table, strings.Join(cols, ","), partial))
		}
		if f["origin"] == "c" {
			named[f["name"]] = fmt.Sprintf("unique=%v partial=%v cols=%s", unique, partial, strings.Join(cols, ","))
		}
	}
	// A rowid-alias INTEGER PRIMARY KEY has no index in SQLite; Postgres's primary key has one.
	for _, l := range pragmaRows(t, db, "PRAGMA table_xinfo("+q+")") {
		if c := sqliteCol(l); c.pk == "1" && rowidAlias(t, db, table) {
			keys = append(keys, fmt.Sprintf("%s(%s) partial=false", table, c.name))
		}
	}
	sort.Strings(keys)
	return named, keys
}

type sqliteColumn struct{ name, typ, notNull, dflt, pk, hidden string }

var xinfoRe = regexp.MustCompile(`^cid=\S+ name=(\S+) type=(.*) notnull=(\d) dflt_value=(.*) pk=(\d+) hidden=(\d+)$`)

func sqliteCol(l string) sqliteColumn {
	m := xinfoRe.FindStringSubmatch(l)
	if m == nil {
		panic("unexpected table_xinfo row: " + l)
	}
	return sqliteColumn{name: m[1], typ: m[2], notNull: m[3], dflt: m[4], pk: m[5], hidden: m[6]}
}

// rowidAlias reports whether the table's primary key is a single INTEGER column, SQLite's rowid alias.
func rowidAlias(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var n int
	var typ string
	if err := db.QueryRow(`SELECT count(*), COALESCE(max(upper(type)), '') FROM pragma_table_info(?) WHERE pk > 0`, table).Scan(&n, &typ); err != nil {
		t.Fatal(err)
	}
	return n == 1 && typ == "INTEGER"
}

var pgDefaultCast = regexp.MustCompile(`::(text|bigint|integer|double precision|bytea)$`)

// normPGDefault renders a Postgres column default the way SQLite's dflt_value shows the same default.
func normPGDefault(d sql.NullString) string {
	if !d.Valid {
		return "<nil>"
	}
	if strings.Contains(d.String, "statement_timestamp()") {
		return "CURRENT_TIMESTAMP"
	}
	return pgDefaultCast.ReplaceAllString(d.String, "")
}

func checkCount(sqlText string) int {
	return len(regexp.MustCompile(`(?i)\bCHECK\s*\(`).FindAllString(sqlText, -1))
}

// TestPostgresSchemaMatchesSQLite proves the Postgres set builds the same schema as the SQLite set under
// the rules in postgres/00001_schema.sql: the same tables; per table the same columns in order with the
// mapped type, nullability and default; the same primary key and unique keys; the same named indexes;
// the same number of CHECK constraints (a count, not an expression match: TestTriggerParity exercises
// what the checks do); and the same triggers. The allowlists above name every
// difference, with its reason. What the triggers and checks do is TestTriggerParity's job.
func TestPostgresSchemaMatchesSQLite(t *testing.T) {
	lite, pg := sqliteStore(t).DB(), pgStore(t).DB()

	var liteObjs []object
	for _, o := range objects(t, lite) {
		if o.name == "goose_db_version" || strings.HasPrefix(o.name, "sqlite_autoindex_goose") {
			continue
		}
		liteObjs = append(liteObjs, o)
	}
	var liteTables []string
	for _, o := range liteObjs {
		if o.typ == "table" && !generated(o, liteObjs) {
			liteTables = append(liteTables, o.name)
		}
	}
	sort.Strings(liteTables)
	pgTables := strs(t, pg, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' AND table_name <> 'goose_db_version'
		ORDER BY table_name`)
	sort.Strings(pgTables)
	if strings.Join(liteTables, " ") != strings.Join(pgTables, " ") {
		t.Fatalf("tables differ:\nsqlite:   %v\npostgres: %v", liteTables, pgTables)
	}

	liteNamed, pgNamed := map[string]string{}, map[string]string{}
	for _, table := range liteTables {
		var tableSQL string
		for _, o := range liteObjs {
			if o.name == table {
				tableSQL = o.sql.String
			}
		}
		lc := pragmaRows(t, lite, "PRAGMA table_xinfo("+quoteIdent(table)+")")
		pc := pgColumns(t, pg, table)

		if table == ftsTable {
			var lv, pv []string
			for _, l := range lc {
				if c := sqliteCol(l); c.hidden == "0" {
					lv = append(lv, c.name)
				}
			}
			for _, c := range pc {
				pv = append(pv, c.name)
			}
			if strings.Join(lv, ",")+",search" != strings.Join(pv, ",") {
				t.Errorf("%s: columns sqlite %v, postgres %v (want the same plus search)", table, lv, pv)
			}
			pn, _ := pgIndexes(t, pg, table)
			for k, v := range pn {
				pgNamed[k] = v
			}
			continue
		}

		if len(lc) != len(pc) {
			t.Errorf("%s: %d columns on sqlite, %d on postgres", table, len(lc), len(pc))
			continue
		}
		alias := rowidAlias(t, lite, table)
		var litePK []string
		pkPos := map[string]string{}
		for i, l := range lc {
			c, p := sqliteCol(l), pc[i]
			where := table + "." + c.name
			if c.name != p.name {
				t.Errorf("%s column %d: sqlite %s, postgres %s", table, i, c.name, p.name)
				continue
			}
			if want := pgTypes[strings.ToUpper(c.typ)]; want != p.typ {
				t.Errorf("%s: sqlite type %q wants postgres %q, got %q", where, c.typ, want, p.typ)
			}
			if p.typ == "text" && p.collation != "C" {
				t.Errorf("%s: text without COLLATE \"C\" (got %q)", where, p.collation)
			}
			if c.pk != "0" {
				pkPos[c.pk] = c.name
			}
			// SQLite lets a non-INTEGER primary key column hold NULL unless it says NOT NULL; Postgres never
			// does. Every live key is non-NULL (S12 counts the rows it copies), so this is not a difference.
			if want := c.notNull == "1" || c.pk != "0"; want != p.notNull {
				t.Errorf("%s: NOT NULL sqlite %v, postgres %v", where, want, p.notNull)
			}
			if _, ok := columnExceptions[where]; ok {
				continue
			}
			if want := alias && c.pk == "1"; want != p.identity {
				t.Errorf("%s: identity on postgres %v, want %v (rowid alias)", where, p.identity, want)
			}
			if got := normPGDefault(p.dflt); !p.identity && got != c.dflt {
				t.Errorf("%s: default sqlite %s, postgres %s", where, c.dflt, got)
			}
		}
		for i := 1; i <= len(pkPos); i++ {
			litePK = append(litePK, pkPos[fmt.Sprint(i)])
		}
		pgPK := strs(t, pg, `SELECT a.attname FROM pg_index i
			JOIN unnest(i.indkey::int2[]) WITH ORDINALITY AS u(k, n) ON true
			JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = u.k
			WHERE i.indrelid = $1::regclass AND i.indisprimary ORDER BY u.n`, table)
		if strings.Join(litePK, ",") != strings.Join(pgPK, ",") {
			t.Errorf("%s: primary key sqlite (%s), postgres (%s)", table, strings.Join(litePK, ","), strings.Join(pgPK, ","))
		}

		ln, lk := sqliteIndexes(t, lite, table)
		pn, pk := pgIndexes(t, pg, table)
		for i, k := range lk {
			for from, to := range uniqueAliases {
				if strings.HasPrefix(k, from+" ") {
					lk[i] = strings.Replace(k, from, to, 1)
				}
			}
		}
		sort.Strings(lk)
		if strings.Join(lk, "; ") != strings.Join(pk, "; ") {
			t.Errorf("%s: unique keys differ:\nsqlite:   %v\npostgres: %v", table, lk, pk)
		}
		for k, v := range ln {
			liteNamed[k] = v
		}
		for k, v := range pn {
			pgNamed[k] = v
		}

		var pgChecks int
		if err := pg.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conrelid = $1::regclass AND contype = 'c'`, table).Scan(&pgChecks); err != nil {
			t.Fatal(err)
		}
		if n := checkCount(tableSQL); n != pgChecks {
			t.Errorf("%s: %d CHECK constraints on sqlite, %d on postgres", table, n, pgChecks)
		}
	}

	for name, v := range liteNamed {
		if _, ok := sqliteOnly[name]; ok {
			continue
		}
		if pv, ok := pgNamed[name]; !ok {
			t.Errorf("index %s is not on postgres", name)
		} else if pv != v {
			t.Errorf("index %s: sqlite %s, postgres %s", name, v, pv)
		}
	}
	for name := range pgNamed {
		if _, ok := liteNamed[name]; !ok {
			if _, allowed := pgOnly[name]; !allowed {
				t.Errorf("postgres index %s is not on sqlite and not on the pgOnly allowlist", name)
			}
		}
	}

	var liteTriggers []string
	for _, o := range liteObjs {
		if _, skip := sqliteOnly[o.name]; o.typ == "trigger" && !skip {
			liteTriggers = append(liteTriggers, o.table+"."+o.name)
		}
	}
	sort.Strings(liteTriggers)
	pgTriggers := strs(t, pg, `SELECT c.relname || '.' || tg.tgname FROM pg_trigger tg JOIN pg_class c ON c.oid = tg.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE NOT tg.tgisinternal AND n.nspname = current_schema()`)
	sort.Strings(pgTriggers)
	seenPGOnly := map[string]bool{}
	kept := pgTriggers[:0]
	for _, tr := range pgTriggers {
		name := tr[strings.Index(tr, ".")+1:]
		if _, ok := pgOnlyTriggers[name]; ok {
			seenPGOnly[name] = true
			continue
		}
		kept = append(kept, tr)
	}
	pgTriggers = kept
	for name := range pgOnlyTriggers {
		if !seenPGOnly[name] {
			t.Errorf("pgOnlyTriggers names %s, which postgres does not have: remove the entry", name)
		}
	}
	if strings.Join(liteTriggers, "\n") != strings.Join(pgTriggers, "\n") {
		t.Errorf("triggers differ:\nsqlite:   %v\npostgres: %v", liteTriggers, pgTriggers)
	}

	for name := range pgOnly {
		if _, ok := pgNamed[name]; !ok {
			t.Errorf("pgOnly names %s, which postgres does not have: remove the entry", name)
		}
	}
	for name := range sqliteOnly {
		found := false
		for _, o := range liteObjs {
			found = found || o.name == name
		}
		if !found {
			t.Errorf("sqliteOnly names %s, which sqlite does not have: remove the entry", name)
		}
	}
}
