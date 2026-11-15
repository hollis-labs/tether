package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
)

// maxParams bounds the bound parameters of one INSERT (SQLite allows 32766, Postgres 65535).
const maxParams = 30000

// rangeResult is one key range of one table, compared or synced.
type rangeResult struct {
	// The range is (Lo, Hi]; an empty Lo is the start, an empty Hi the end.
	Lo      string `json:"lo,omitempty"`
	Hi      string `json:"hi,omitempty"`
	Rows    int64  `json:"rows"`    // source rows in the range
	Changed bool   `json:"changed"` // the target range differed from the source
}

// TableReport is what one table's sync or verification found.
type TableReport struct {
	Table      string        `json:"table"`
	Rows       int64         `json:"rows"`
	Ranges     int           `json:"ranges"`
	Written    int           `json:"written_ranges"`              // ranges copied or re-copied (on a fresh target, every non-empty one)
	Mismatched []rangeResult `json:"mismatched_ranges,omitempty"` // verification only
	Digest     string        `json:"digest"`
	Seconds    float64       `json:"seconds"`
}

// walker compares (and, when syncing, repairs) one table range by range. A range is the source rows
// after the previous range's last key, up to and including this range's last key; the last range is
// open-ended, so keys the target holds beyond the source's last key are found too.
type walker struct {
	src   *sql.DB
	tgt   *store.Handle
	sp    tableSpec
	batch int
	sync  bool // repair differing ranges; otherwise only report them
	pg    bool
}

func (w *walker) run(ctx context.Context) (TableReport, error) {
	started := time.Now()
	rep := TableReport{Table: w.sp.name}
	whole := newRowDigest()
	keys := w.sp.keyIndexes()
	kinds := w.sp.kinds()
	var last []any // the previous range's last key; nil at the start
	for {
		rows, err := w.readSource(ctx, last)
		if err != nil {
			return rep, fmt.Errorf("%s: read source: %w", w.sp.name, err)
		}
		var hi []any
		if len(rows) == w.batch {
			hi = keyOf(rows[len(rows)-1], keys)
		}
		// hi == nil marks the final, open-ended range.
		src := newRowDigest()
		for _, r := range rows {
			src.add(kinds, r)
			whole.add(kinds, r)
		}
		rr := rangeResult{Lo: renderKey(last), Hi: renderKey(hi), Rows: int64(len(rows))}
		changed, err := w.compare(ctx, last, hi, src.sum(), rows)
		if err != nil {
			return rep, fmt.Errorf("%s: range (%s, %s]: %w", w.sp.name, rr.Lo, rr.Hi, err)
		}
		rr.Changed = changed
		rep.Ranges++
		rep.Rows += rr.Rows
		if changed {
			if w.sync {
				rep.Written++
			} else {
				rep.Mismatched = append(rep.Mismatched, rr)
			}
		}
		if hi == nil {
			break
		}
		last = hi
	}
	rep.Digest = whole.sum()
	rep.Seconds = time.Since(started).Seconds()
	return rep, nil
}

// readSource reads the next range: up to batch rows after last, in key order, transformed.
func (w *walker) readSource(ctx context.Context, last []any) ([][]any, error) {
	//nolint:gosec // table and column names come from the schema, never from input; values are bound
	q := `SELECT ` + w.sp.selectList(true) + ` FROM ` + quote(w.sp.name)
	var args []any
	if last != nil {
		q += ` WHERE ` + w.sp.keyBound(">")
		args = append(args, last...)
	}
	q += ` ORDER BY ` + w.sp.keyOrder() + ` LIMIT ?`
	args = append(args, w.batch)
	rows, err := w.src.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out, err := scanAll(rows, len(w.sp.cols))
	if err != nil {
		return nil, err
	}
	// Derived columns were read as the source rowid; apply the transform.
	tr := copyTransforms[w.sp.name]
	for i, c := range w.sp.cols {
		if !c.derived {
			continue
		}
		for _, r := range out {
			rowid, ok := r[i].(int64)
			if !ok {
				return nil, fmt.Errorf("%s: rowid is %T", w.sp.name, r[i])
			}
			r[i] = tr.derived[c.name](rowid)
		}
	}
	return out, nil
}

// compare digests the target's rows in the range and, when syncing and they differ, replaces them with
// the source rows, all in one class transaction.
func (w *walker) compare(ctx context.Context, lo, hi []any, srcDigest string, srcRows [][]any) (bool, error) {
	var changed bool
	err := w.tgt.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		where, args := w.rangeWhere(lo, hi)
		rows, err := tx.QueryContext(ctx, `SELECT `+w.sp.selectList(false)+` FROM `+quote(w.sp.name)+where+` ORDER BY `+w.sp.keyOrder(), args...)
		if err != nil {
			return err
		}
		tgtRows, err := scanAll(rows, len(w.sp.cols))
		_ = rows.Close()
		if err != nil {
			return err
		}
		d := newRowDigest()
		for _, r := range tgtRows {
			d.add(w.sp.kinds(), r)
		}
		changed = d.sum() != srcDigest
		if !changed || !w.sync {
			return nil
		}
		if len(tgtRows) > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+quote(w.sp.name)+where, args...); err != nil {
				return err
			}
		}
		return w.insert(ctx, tx, srcRows)
	})
	return changed, err
}

func (w *walker) rangeWhere(lo, hi []any) (string, []any) {
	var conds []string
	var args []any
	if lo != nil {
		conds = append(conds, w.sp.keyBound(">"))
		args = append(args, lo...)
	}
	if hi != nil {
		conds = append(conds, w.sp.keyBound("<="))
		args = append(args, hi...)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return ` WHERE ` + strings.Join(conds, " AND "), args
}

// insert writes rows with explicit values for every column, the rowid included on SQLite.
func (w *walker) insert(ctx context.Context, tx store.Tx, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	names := make([]string, len(w.sp.cols))
	for i, c := range w.sp.cols {
		names[i] = quote(c.name)
		if c.name == rowidCol {
			names[i] = "rowid"
		}
	}
	tuple := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ") + ")"
	per := maxParams / len(names)
	if per < 1 {
		per = 1
	}
	for start := 0; start < len(rows); start += per {
		end := min(start+per, len(rows))
		tuples := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*len(names))
		for _, r := range rows[start:end] {
			tuples = append(tuples, tuple)
			for i, v := range r {
				args = append(args, w.bind(w.sp.cols[i].kind, v))
			}
		}
		//nolint:gosec // table and column names come from the schema, never from input; values are bound
		q := `INSERT INTO ` + quote(w.sp.name) + ` (` + strings.Join(names, ", ") + `) VALUES ` + strings.Join(tuples, ", ")
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}

// bind converts a value for the target. SQLite keeps every value exactly as the source stored it. On
// Postgres a bytea column takes bytes and a text column takes a string; any other mismatch is left to
// fail the insert, so it is reported rather than coerced.
func (w *walker) bind(k kind, v any) any {
	if !w.pg {
		return v
	}
	switch x := v.(type) {
	case string:
		if k == kindBlob {
			return []byte(x)
		}
	case []byte:
		if k == kindText {
			return string(x)
		}
	}
	return v
}

func scanAll(rows *sql.Rows, n int) ([][]any, error) {
	var out [][]any
	for rows.Next() {
		vals := make([]any, n)
		ptrs := make([]any, n)
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = append([]byte(nil), b...) // the driver may reuse the buffer
			}
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

func keyOf(row []any, idx []int) []any {
	k := make([]any, len(idx))
	for i, j := range idx {
		k[i] = row[j]
	}
	return k
}

// renderKey prints a key for the report: values only, never row content.
func renderKey(k []any) string {
	if k == nil {
		return ""
	}
	parts := make([]string, len(k))
	for i, v := range k {
		if b, ok := v.([]byte); ok {
			v = string(b)
		}
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ",")
}
