package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
)

// ranged reports whether a table is walked in fixed-width ranges of a single integer key: the key is the
// row's identity (an INTEGER PRIMARY KEY, the rowid itself on SQLite), so no rowid lives outside its key
// range and ranges are independent of each other. Every other table is walked by the sequential walker.
func (sp tableSpec) ranged() bool {
	if len(sp.key) != 1 || sp.rowid {
		return false
	}
	for _, c := range sp.cols {
		if c.name == sp.key[0] {
			return c.kind == kindInt
		}
	}
	return false
}

// rangedWalker walks a ranged table in fixed-width key ranges (lo, lo+width], with workers in parallel.
// Fixed boundaries are the same in every snapshot, so a range that did not change between the bulk copy
// and the final copy has the same digest in both, whatever was inserted or deleted elsewhere. stored holds
// the target's range digests from the previous verified run (nil for none): a range whose source digest
// equals its stored digest is equal without reading the target, so a final run reads only the source for
// the unchanged ranges.
type rangedWalker struct {
	*walker
	width   int64
	workers int
	stored  map[int64]string
}

// rangeOut is one range's result.
type rangeOut struct {
	lo      int64
	rows    int64
	digest  string // the source rows' digest ("" for an empty range)
	changed bool
}

func (w *rangedWalker) run(ctx context.Context) (TableReport, map[int64]string, error) {
	started := time.Now()
	rep := TableReport{Table: w.sp.name}
	lo, hi, ok, err := w.extent(ctx)
	if err != nil || !ok {
		rep.Digest = fmt.Sprintf("%x", sha256.Sum256(nil))
		rep.Seconds = time.Since(started).Seconds()
		return rep, map[int64]string{}, err
	}
	// Keys near the ends of int64 would overflow the range arithmetic; no Tether table holds such keys.
	if lo <= math.MinInt64+w.width || hi >= math.MaxInt64-w.width {
		return rep, nil, fmt.Errorf("%s: keys %d..%d are too close to the int64 limits for ranged copying", w.sp.name, lo, hi)
	}
	first := floorDiv(lo-1, w.width) * w.width // the range holding lo is (first, first+width]
	jobs := make(chan int64)
	results := make(chan rangeOut, w.workers*2)
	errs := make(chan error, w.workers)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < w.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				out, err := w.one(ctx, r)
				if err != nil {
					errs <- fmt.Errorf("%s: range (%d, %d]: %w", w.sp.name, r, r+w.width, err)
					cancel()
					return
				}
				results <- out
			}
		}()
	}
	// Collect while the workers run (results is bounded), and feed the ranges lazily: key gaps cost one
	// empty range each, never a precomputed list.
	var outs []rangeOut
	collected := make(chan struct{})
	go func() {
		for o := range results {
			outs = append(outs, o)
		}
		close(collected)
	}()
feed:
	for r := first; r < hi; r += w.width {
		select {
		case jobs <- r:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	close(results)
	<-collected
	select {
	case err := <-errs:
		return rep, nil, err
	default:
	}
	// A canceled parent context stops the feed early: never return the partial walk as a result.
	if err := ctx.Err(); err != nil {
		return rep, nil, err
	}
	sort.Slice(outs, func(i, j int) bool { return outs[i].lo < outs[j].lo })
	digests := map[int64]string{}
	whole := sha256.New()
	for _, o := range outs {
		rep.Ranges++
		rep.Rows += o.rows
		if o.rows > 0 {
			digests[o.lo] = o.digest
			whole.Write(binary.AppendVarint(nil, o.lo))
			whole.Write([]byte(o.digest))
		}
		if !o.changed {
			continue
		}
		b := bounds{lo: []any{o.lo}, hi: []any{o.lo + w.width}}
		if w.sync {
			rep.Written++
			rep.written = append(rep.written, b)
		} else {
			rep.Mismatched = append(rep.Mismatched, rangeResult{Lo: fmt.Sprint(o.lo), Hi: fmt.Sprint(o.lo + w.width), Rows: o.rows, Changed: true})
		}
	}
	rep.Digest = fmt.Sprintf("%x", whole.Sum(nil))
	rep.Seconds = time.Since(started).Seconds()
	return rep, digests, nil
}

// extent is the smallest and largest key on either side.
func (w *rangedWalker) extent(ctx context.Context) (lo, hi int64, ok bool, err error) {
	k := quote(w.sp.key[0])
	q := `SELECT MIN(` + k + `), MAX(` + k + `) FROM ` + quote(w.sp.name)
	var sMin, sMax, tMin, tMax *int64
	if err := w.src.QueryRowContext(ctx, q).Scan(&sMin, &sMax); err != nil {
		return 0, 0, false, err
	}
	if err := w.tgt.Reader().QueryRowContext(ctx, q).Scan(&tMin, &tMax); err != nil {
		return 0, 0, false, err
	}
	for _, v := range []*int64{sMin, tMin} {
		if v != nil && (!ok || *v < lo) {
			lo, ok = *v, true
		}
	}
	for _, v := range []*int64{sMax, tMax} {
		if v != nil && *v > hi {
			hi = *v
		}
	}
	return lo, hi, ok, nil
}

// one compares (and, syncing, repairs) the range (r, r+width].
func (w *rangedWalker) one(ctx context.Context, r int64) (rangeOut, error) {
	lo, hi := []any{r}, []any{r + w.width}
	where, args := w.rangeWhere(lo, hi)
	//nolint:gosec // names come from the schema; values are bound
	rows, err := w.src.QueryContext(ctx, `SELECT `+w.sp.selectList(true)+` FROM `+quote(w.sp.name)+where+` ORDER BY `+w.sp.keyOrder(), args...)
	if err != nil {
		return rangeOut{}, err
	}
	srcRows, err := scanAll(rows, len(w.sp.cols))
	_ = rows.Close()
	if err != nil {
		return rangeOut{}, err
	}
	if err := w.derive(srcRows); err != nil {
		return rangeOut{}, err
	}
	d := newRowDigest()
	for _, row := range srcRows {
		d.add(w.sp.kinds(), row)
	}
	out := rangeOut{lo: r, rows: int64(len(srcRows))}
	if len(srcRows) > 0 {
		out.digest = d.sum()
	}
	if stored, ok := w.stored[r]; ok && w.stored != nil && stored == out.digest {
		return out, nil // equal to what the previous verified run left in the target
	}
	// Read the target's range through the read pool, outside any write transaction, so ranges compare in
	// parallel; only a differing range takes the writer.
	//nolint:gosec // names come from the schema; values are bound
	trows, err := w.tgt.Reader().QueryContext(ctx, `SELECT `+w.sp.selectList(false)+` FROM `+quote(w.sp.name)+where+` ORDER BY `+w.sp.keyOrder(), args...)
	if err != nil {
		return rangeOut{}, err
	}
	tgtRows, err := scanAll(trows, len(w.sp.cols))
	_ = trows.Close()
	if err != nil {
		return rangeOut{}, err
	}
	td := newRowDigest()
	for _, row := range tgtRows {
		td.add(w.sp.kinds(), row)
	}
	if td.sum() == d.sum() {
		return out, nil
	}
	out.changed = true
	if !w.sync {
		return out, nil
	}
	err = w.tgt.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if len(tgtRows) > 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+quote(w.sp.name)+where, args...); err != nil {
				return err
			}
		}
		return w.insert(ctx, tx, srcRows)
	})
	return out, err
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}
