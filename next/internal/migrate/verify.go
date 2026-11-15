package migrate

import (
	"context"
	"fmt"
)

// verify re-walks every table without repairing, comparing each key range of the target with the
// source (digests over ordered canonical rows), and checks counts, messages.seq, and on SQLite the
// carried sequences, foreign key violations (exactly the source's) and integrity.
func (e *Engine) verify(ctx context.Context, specs []tableSpec, batch int, rep *Report, progress func(string, ...any)) error {
	for _, sp := range specs {
		tr, err := e.walk(ctx, sp, batch, false)
		if err != nil {
			return err
		}
		rep.Verify = append(rep.Verify, tr)
		for _, m := range tr.Mismatched {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: range (%s, %s] differs", sp.name, m.Lo, m.Hi))
		}
		var src, tgt int64
		if err := e.src.QueryRowContext(ctx, `SELECT count(*) FROM `+quote(sp.name)).Scan(&src); err != nil {
			return err
		}
		if err := e.raw.Reader().QueryRowContext(ctx, `SELECT count(*) FROM `+quote(sp.name)).Scan(&tgt); err != nil {
			return err
		}
		rep.Counts[sp.name] = [2]int64{src, tgt}
		if src != tgt {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %d rows in the source, %d in the target", sp.name, src, tgt))
		}
		progress("verify %-28s %9d rows %5d ranges %5d differ %7.2fs", sp.name, tr.Rows, tr.Ranges, len(tr.Mismatched), tr.Seconds)
	}

	rep.MessagesSeq = "not copied"
	for _, sp := range specs {
		if sp.name != "messages" {
			continue
		}
		rep.MessagesSeq = "ok"
		if err := VerifyMessagesSeq(ctx, e.src, e.raw.Reader()); err != nil {
			rep.MessagesSeq = err.Error()
			rep.Problems = append(rep.Problems, err.Error())
		}
	}

	if !e.sqlite() {
		return nil
	}
	rep.Sequences = map[string][2]int64{}
	rep.ForeignKeys = map[string][2]int64{}
	for _, sp := range specs {
		var src, tgt int64
		srcErr := e.src.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = ?`, sp.name).Scan(&src)
		tgtErr := e.raw.Reader().QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = ?`, sp.name).Scan(&tgt)
		if srcErr == nil || tgtErr == nil {
			rep.Sequences[sp.name] = [2]int64{src, tgt}
			if src != tgt {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s: sqlite_sequence %d in the source, %d in the target", sp.name, src, tgt))
			}
		}
		// The old store runs with foreign keys off and holds orphans: the copy must have exactly the
		// source's violations, none added and none dropped.
		if err := e.src.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check(?)`, sp.name).Scan(&src); err != nil {
			return err
		}
		if err := e.raw.Reader().QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check(?)`, sp.name).Scan(&tgt); err != nil {
			return err
		}
		if src != 0 || tgt != 0 {
			rep.ForeignKeys[sp.name] = [2]int64{src, tgt}
		}
		if src != tgt {
			rep.Problems = append(rep.Problems, fmt.Sprintf("%s: %d foreign key violations in the source, %d in the target", sp.name, src, tgt))
		}
	}
	if err := e.raw.Reader().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&rep.Integrity); err != nil {
		return err
	}
	if rep.Integrity != "ok" {
		rep.Problems = append(rep.Problems, "integrity_check: "+rep.Integrity)
	}
	return nil
}
