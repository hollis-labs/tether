package migrate

import (
	"context"
	"fmt"
	"strings"
)

// verify compares each key range of the target with the source (digests over ordered canonical rows),
// and checks counts, messages.seq, and on SQLite the carried sequences, foreign key violations (exactly
// the source's) and integrity. With the copy walk's reports (after a run), it rechecks only the ranges the
// copy wrote: the others were proven equal by the copy walk's own digests. Without them (Verify), it walks
// every range.
func (e *Engine) verify(ctx context.Context, specs []tableSpec, batch int, rep *Report, progress func(string, ...any), copyReports []TableReport) error {
	for i, sp := range specs {
		var tr TableReport
		var err error
		if copyReports != nil {
			tr, err = e.walker(sp, batch, false).recheck(ctx, copyReports[i].written)
			tr.Digest = copyReports[i].Digest
			rep.VerifyMode = "incremental"
		} else {
			tr, err = e.walk(ctx, sp, batch, false)
			rep.VerifyMode = "full"
		}
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
	rep.ForeignKeysDeferred = map[string]int64{}
	copied := make([]any, 0, len(specs))
	in := make([]string, 0, len(specs))
	for _, sp := range specs {
		copied = append(copied, sp.name)
		in = append(in, "?")
	}
	// A violation whose parent table is not copied yet (a later family) is not comparable until that
	// family lands: it is reported as deferred, not compared.
	parentIn := ` AND parent IN (` + strings.Join(in, ", ") + `)`
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
		args := append([]any{sp.name}, copied...)
		if err := e.src.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check(?) WHERE 1 = 1`+parentIn, args...).Scan(&src); err != nil {
			return err
		}
		if err := e.raw.Reader().QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check(?) WHERE 1 = 1`+parentIn, args...).Scan(&tgt); err != nil {
			return err
		}
		var deferred int64
		if err := e.raw.Reader().QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check(?) WHERE NOT (1 = 0`+strings.Replace(parentIn, " AND ", " OR ", 1)+`)`, args...).Scan(&deferred); err != nil {
			return err
		}
		if deferred > 0 {
			rep.ForeignKeysDeferred[sp.name] = deferred
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
