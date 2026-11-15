package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	gostore "github.com/hollis-labs/libs/core/store"
)

// column is one copied column: its name and its kind on the target.
type column struct {
	name string
	kind kind
	// derived columns are computed from the source row by the table's transform, not read from a source
	// column of the same name.
	derived bool
}

// tableSpec is how one table is copied and compared.
type tableSpec struct {
	name string
	cols []column // the target's columns in order (generated columns excluded), plus the rowid on SQLite
	key  []string // the primary key, in key order: the order rows are copied and ranged in
	// rowid: the SQLite target carries the source's implicit rowid (every table without an INTEGER
	// PRIMARY KEY alias). Postgres has none; the tables whose rowid is a contract carry it in a column
	// (messages.seq).
	rowid bool
	// sqliteTarget flags the target dialect; it decides how timestamps are read back (selectList).
	sqliteTarget bool
	// ts names the columns declared DATE, DATETIME or TIMESTAMP, which the SQLite driver would return as
	// time.Time instead of their stored text.
	ts map[string]bool
}

const rowidCol = "__rowid"

// copyTransform is a table whose new shape differs from the old one (plan section 7.1). derived computes
// each added column from the source row's rowid.
type copyTransform struct {
	why     string
	derived map[string]func(rowid int64) any
}

// copyTransforms is the registry. It starts with messages.seq (plan section 13.1).
var copyTransforms = map[string]copyTransform{
	"messages": {
		why:     "seq is the old rowid: the mailbox watch cursor clients hold (VerifyMessagesSeq)",
		derived: map[string]func(int64) any{"seq": func(rowid int64) any { return rowid }},
	},
}

// targetColumns lists a target table's columns with their declared types, in order, without generated
// columns (forum_post_fts.search on Postgres).
func targetColumns(ctx context.Context, st *gostore.Store, table string) ([][2]string, error) {
	var q string
	switch st.Dialect().Name() {
	case gostore.DriverSQLite:
		q = `SELECT name, type FROM pragma_table_xinfo(?) WHERE hidden = 0 ORDER BY cid`
	default:
		q = `SELECT column_name, CASE WHEN data_type = 'USER-DEFINED' THEN udt_name ELSE data_type END
			FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND is_generated = 'NEVER'
			ORDER BY ordinal_position`
	}
	rows, err := st.Reader().QueryContext(ctx, q, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out [][2]string
	for rows.Next() {
		var c [2]string
		if err := rows.Scan(&c[0], &c[1]); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("target has no table %s", table)
	}
	return out, nil
}

// sourceTable reads a source table's columns, primary key and whether its key is the rowid alias.
func sourceTable(ctx context.Context, src *sql.DB, table string) (cols map[string]string, key []string, rowidAlias bool, err error) {
	rows, err := src.QueryContext(ctx, `SELECT name, type, pk FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, nil, false, err
	}
	defer func() { _ = rows.Close() }()
	cols = map[string]string{}
	pk := map[int]string{}
	var pkType string
	for rows.Next() {
		var name, typ string
		var pos int
		if err := rows.Scan(&name, &typ, &pos); err != nil {
			return nil, nil, false, err
		}
		cols[name] = typ
		if pos > 0 {
			pk[pos] = name
			pkType = typ
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}
	if len(cols) == 0 {
		return nil, nil, false, fmt.Errorf("source has no table %s", table)
	}
	for i := 1; i <= len(pk); i++ {
		key = append(key, pk[i])
	}
	return cols, key, len(key) == 1 && strings.EqualFold(pkType, "INTEGER"), nil
}

// specFor builds the copy spec of one table from both schemas. Every target column must come from a
// source column of the same name or from the table's transform: a column the source lacks is an error,
// never a silent default.
func specFor(ctx context.Context, src *sql.DB, tgt *gostore.Store, table string) (tableSpec, error) {
	scols, key, alias, err := sourceTable(ctx, src, table)
	if err != nil {
		return tableSpec{}, err
	}
	if len(key) == 0 {
		return tableSpec{}, fmt.Errorf("%s has no primary key: S12a copies keyed tables only", table)
	}
	tcols, err := targetColumns(ctx, tgt, table)
	if err != nil {
		return tableSpec{}, err
	}
	tr := copyTransforms[table]
	sp := tableSpec{name: table, key: key, sqliteTarget: tgt.Dialect().Name() == gostore.DriverSQLite, ts: map[string]bool{}}
	for name, typ := range scols {
		switch strings.ToUpper(typ) {
		case "DATE", "DATETIME", "TIMESTAMP":
			sp.ts[name] = true
		}
	}
	for _, c := range tcols {
		_, fromSource := scols[c[0]]
		_, derived := tr.derived[c[0]]
		if !fromSource && !derived {
			return tableSpec{}, fmt.Errorf("%s.%s: the target column has no source column and no transform", table, c[0])
		}
		sp.cols = append(sp.cols, column{name: c[0], kind: kindOf(c[1]), derived: derived && !fromSource})
	}
	for name := range scols {
		found := false
		for _, c := range tcols {
			found = found || c[0] == name
		}
		if !found {
			return tableSpec{}, fmt.Errorf("%s.%s: the source column has no target column", table, name)
		}
	}
	sp.rowid = sp.sqliteTarget && !alias
	if sp.rowid {
		sp.cols = append(sp.cols, column{name: rowidCol, kind: kindInt})
	}
	return sp, nil
}

// keyIndexes returns the positions of the key columns in sp.cols.
func (sp tableSpec) keyIndexes() []int {
	var out []int
	for _, k := range sp.key {
		for i, c := range sp.cols {
			if c.name == k {
				out = append(out, i)
			}
		}
	}
	return out
}

func (sp tableSpec) kinds() []kind {
	out := make([]kind, len(sp.cols))
	for i, c := range sp.cols {
		out[i] = c.kind
	}
	return out
}

func quote(id string) string { return `"` + strings.ReplaceAll(id, `"`, `""`) + `"` }

// selectList renders the column list for reading sp from the source or the target. SQLite returns a
// TIMESTAMP- or DATETIME-declared column as time.Time; `col || ''` reads the stored text instead, byte for
// byte (the CASE keeps a non-text value as it is).
func (sp tableSpec) selectList(source bool) string {
	parts := make([]string, 0, len(sp.cols))
	for _, c := range sp.cols {
		switch {
		case c.name == rowidCol:
			parts = append(parts, "rowid")
		case c.derived && source:
			parts = append(parts, "rowid") // the transform derives it from the source rowid
		case sp.ts[c.name] && (source || sp.sqliteTarget):
			parts = append(parts, fmt.Sprintf("CASE WHEN typeof(%[1]s) = 'text' THEN %[1]s || '' ELSE %[1]s END", quote(c.name)))
		default:
			parts = append(parts, quote(c.name))
		}
	}
	return strings.Join(parts, ", ")
}

// keyOrder renders ORDER BY over the key.
func (sp tableSpec) keyOrder() string {
	parts := make([]string, len(sp.key))
	for i, k := range sp.key {
		parts[i] = quote(k)
	}
	return strings.Join(parts, ", ")
}

// keyBound renders "(k1, k2) >= (?, ?)" (or "<") for a range bound.
func (sp tableSpec) keyBound(op string) string {
	cols := make([]string, len(sp.key))
	ph := make([]string, len(sp.key))
	for i, k := range sp.key {
		cols[i], ph[i] = quote(k), "?"
	}
	if len(sp.key) == 1 {
		return cols[0] + " " + op + " ?"
	}
	return "(" + strings.Join(cols, ", ") + ") " + op + " (" + strings.Join(ph, ", ") + ")"
}
