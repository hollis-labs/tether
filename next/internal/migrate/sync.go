package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

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
	// written holds the bounds of the ranges a sync walk wrote, for the incremental verification.
	written []bounds
}

// bounds is one key range, (lo, hi]; nil is open.
type bounds struct{ lo, hi []any }

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
	// conversions counts, per table.column, values the Postgres target stores in another class; convMu
	// guards it (parallel workers share it).
	conversions map[string]int64
	convMu      *sync.Mutex
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
				rep.written = append(rep.written, bounds{lo: last, hi: hi})
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
	if err := w.derive(out); err != nil {
		return nil, err
	}
	return out, nil
}

// derive prepares rows read from the source: the Postgres class conversions, then the derived columns
// (read as the source rowid) through the table's transform.
func (w *walker) derive(out [][]any) error {
	w.convert(out)
	tr := copyTransforms[w.sp.name]
	for i, c := range w.sp.cols {
		if !c.derived {
			continue
		}
		for _, r := range out {
			rowid, ok := r[i].(int64)
			if !ok {
				return fmt.Errorf("%s: rowid is %T", w.sp.name, r[i])
			}
			r[i] = tr.derived[c.name](rowid)
		}
	}
	return nil
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
		if err := w.freeRowids(ctx, tx, srcRows); err != nil {
			return err
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

// convert applies, to rows read from the source, the class conversions a Postgres target forces: a bytea
// column holds bytes and a text column a string, whatever class SQLite's affinity let the source store.
// Each conversion is counted per table.column in the report; the rows are then digested and written as
// converted, so verification compares like with like and the conversion is recorded, not hidden. SQLite
// keeps every value exactly as the source stored it. Any other mismatch (text in an integer column) is
// left to fail the insert, so it is reported rather than coerced.
func (w *walker) convert(rows [][]any) {
	if !w.pg {
		return
	}
	for _, r := range rows {
		for i, c := range w.sp.cols {
			switch x := r[i].(type) {
			case string:
				if c.kind == kindBlob {
					r[i] = []byte(x)
					w.count(c)
				}
			case []byte:
				if c.kind == kindText {
					r[i] = string(x)
					w.count(c)
				}
			}
		}
	}
}

// count records a conversion once per copied value: the copy walk counts, the verify walk does not.
func (w *walker) count(c column) {
	if w.sync && w.conversions != nil {
		w.convMu.Lock()
		w.conversions[w.sp.name+"."+c.name]++
		w.convMu.Unlock()
	}
}

// bind passes a value to the driver. A non-nil empty byte slice stays an empty blob (X”), never NULL.
func (w *walker) bind(_ kind, v any) any {
	if b, ok := v.([]byte); ok && b != nil && len(b) == 0 {
		return []byte{}
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
				// The driver may reuse the buffer: copy it, keeping an empty blob empty and non-nil (a
				// nil slice would bind as NULL).
				c := make([]byte, len(b))
				copy(c, b)
				vals[i] = c
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

// freeRowids deletes target rows outside this range that hold a rowid the range's source rows carry. A
// rowid freed by a delete in the source can be taken by a new row with a key in an earlier range; the
// target row holding it (keyed in a later range) goes now, and its own range is re-copied when the walk
// reaches it, since its digest then differs.
func (w *walker) freeRowids(ctx context.Context, tx store.Tx, rows [][]any) error {
	if !w.sp.rowid || len(rows) == 0 {
		return nil
	}
	idx := len(w.sp.cols) - 1 // the rowid is the last column (specFor)
	for start := 0; start < len(rows); start += maxParams {
		end := min(start+maxParams, len(rows))
		args := make([]any, 0, end-start)
		for _, r := range rows[start:end] {
			args = append(args, r[idx])
		}
		//nolint:gosec // the table name comes from the schema; the rowids are bound
		q := `DELETE FROM ` + quote(w.sp.name) + ` WHERE rowid IN (` + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + `)`
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}

// recheck compares only the given ranges of the target with the source, after a sync wrote them. Every
// other range was proven equal by its digest during the sync walk, and nothing else writes the target in
// between (migrate-from holds it), so this completes the verification without a second pass over the
// unchanged ranges (plan section 7.3, item 3). The ranges are compared by workers in parallel, reading
// the target through the read pool; mismatches are reported in key order.
func (w *walker) recheck(ctx context.Context, ranges []bounds, workers int) (TableReport, error) {
	started := time.Now()
	rep := TableReport{Table: w.sp.name}
	type result struct {
		rows     int64
		mismatch bool
		err      error
	}
	results := make([]result, len(ranges))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < max(workers, 1); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				n, differs, err := w.rangeDiffers(ctx, ranges[j])
				results[j] = result{rows: n, mismatch: differs, err: err}
			}
		}()
	}
	for j := range ranges {
		jobs <- j
	}
	close(jobs)
	wg.Wait()
	for j, r := range results {
		if r.err != nil {
			return rep, fmt.Errorf("%s: range (%s, %s]: %w", w.sp.name, renderKey(ranges[j].lo), renderKey(ranges[j].hi), r.err)
		}
		rep.Ranges++
		rep.Rows += r.rows
		if r.mismatch {
			rep.Mismatched = append(rep.Mismatched, rangeResult{Lo: renderKey(ranges[j].lo), Hi: renderKey(ranges[j].hi), Rows: r.rows, Changed: true})
		}
	}
	rep.Seconds = time.Since(started).Seconds()
	return rep, nil
}

// rangeDiffers reads one range on both sides (the target through the read pool) and compares digests.
func (w *walker) rangeDiffers(ctx context.Context, b bounds) (int64, bool, error) {
	where, args := w.rangeWhere(b.lo, b.hi)
	read := func(q gostore.Querier, source bool) ([][]any, error) {
		//nolint:gosec // names come from the schema; values are bound
		rows, err := q.QueryContext(ctx, `SELECT `+w.sp.selectList(source)+` FROM `+quote(w.sp.name)+where+` ORDER BY `+w.sp.keyOrder(), args...)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		return scanAll(rows, len(w.sp.cols))
	}
	srcRows, err := read(sourceQuerier{w.src}, true)
	if err != nil {
		return 0, false, err
	}
	if err := w.derive(srcRows); err != nil {
		return 0, false, err
	}
	tgtRows, err := read(w.tgt.Reader(), false)
	if err != nil {
		return 0, false, err
	}
	sd, td := newRowDigest(), newRowDigest()
	for _, r := range srcRows {
		sd.add(w.sp.kinds(), r)
	}
	for _, r := range tgtRows {
		td.add(w.sp.kinds(), r)
	}
	return int64(len(srcRows)), sd.sum() != td.sum(), nil
}

// sourceQuerier adapts the source *sql.DB to store.Querier for rangeDiffers (no rebinding: the source is
// SQLite, and its queries use "?").
type sourceQuerier struct{ db *sql.DB }

func (s sourceQuerier) ExecContext(ctx context.Context, q string, a ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, q, a...)
}
func (s sourceQuerier) QueryContext(ctx context.Context, q string, a ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, q, a...)
}
func (s sourceQuerier) QueryRowContext(ctx context.Context, q string, a ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, q, a...)
}
func (s sourceQuerier) Dialect() gostore.Dialect {
	d, _ := gostore.DialectFor(gostore.DriverSQLite)
	return d
}
