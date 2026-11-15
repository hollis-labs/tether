package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/substrate/modules/database"

	"github.com/hollis-labs/tether/next/internal/migrate"
)

// migrateFromCmd is `tether migrate-from` (plan section 7.2): copy an old-layout database into this data
// directory's database and prove the copy. It never opens the live file: the operator gives a copy (the
// daily backup's, or one taken with the old hub stopped), and migrate.CheckSource refuses anything that
// may be live.
func (t *tether) migrateFromCmd(ctx context.Context) *cobra.Command {
	var oldDB, phase, reportPath string
	var batch, workers int
	var verifyOnly, precheck bool
	var sourceCheck, targetCheck, family string
	cmd := &cobra.Command{
		Use:   "migrate-from",
		Short: "Copy an old Tether database (a copy, never the live file) into this one and verify it",
		Long: `Copy an old Tether database into this data directory's database and prove the copy.

--phase bulk copies before the window; the database never serves after a bulk run alone.
--phase final syncs every table against a copy of the stopped old database and, when it verifies,
marks the database complete. A complete database is never rewritten: --verify re-checks it against
a copy and writes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !verifyOnly && !precheck && phase == "" {
				return errors.New("--phase is required: bulk (before the window) or final (in the window)")
			}
			src, err := migrate.CheckSource(oldDB)
			if err != nil {
				return err
			}
			db, err := migrate.OpenSource(src)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			if precheck {
				findings, err := migrate.New(db, src, database.From(t.app)).Precheck(ctx)
				if err != nil {
					return err
				}
				failed := 0
				for _, f := range findings {
					status := "ok"
					if !f.OK {
						status, failed = "FAIL", failed+1
					}
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "precheck %-16s %-4s %s\n", f.Check, status, f.Detail)
				}
				if failed > 0 {
					return fmt.Errorf("migrate-from: %d pre-check(s) failed; act on them before the final copy", failed)
				}
				return nil
			}
			tables, ok := migrate.Families[family]
			if !ok {
				return fmt.Errorf("--family must be a, b, c or all, not %q", family)
			}
			out := cmd.OutOrStdout()
			e := migrate.New(db, src, database.From(t.app))
			opt := migrate.Options{
				Tables:      tables,
				Phase:       migrate.Phase(phase),
				Batch:       batch,
				SourceCheck: sourceCheck,
				TargetCheck: targetCheck,
				Workers:     workers,
				Progress: func(format string, args ...any) {
					_, _ = fmt.Fprintf(out, format+"\n", args...)
				},
			}
			var rep *migrate.Report
			if verifyOnly {
				rep, err = e.Verify(ctx, opt)
			} else {
				rep, err = e.Run(ctx, opt)
			}
			if err != nil {
				return err
			}
			if reportPath != "" {
				b, err := json.MarshalIndent(rep, "", "  ")
				if err != nil {
					return err
				}
				if err := os.WriteFile(reportPath, append(b, '\n'), 0o600); err != nil {
					return err
				}
			}
			if verifyOnly {
				_, _ = fmt.Fprintf(out, "verify only (nothing written); verify %.1fs\n", rep.VerifySeconds)
			} else {
				_, _ = fmt.Fprintf(out, "mark %s; copy %.1fs, verify %.1fs, source hash %.1fs; source unchanged: %v\n",
					rep.Mark, rep.CopySeconds, rep.VerifySeconds, rep.HashSeconds, rep.SourceUnchanged)
			}
			for col, n := range rep.Conversions {
				_, _ = fmt.Fprintf(out, "converted %s: %d values (storage class changed for the Postgres target)\n", col, n)
			}
			if !rep.OK() {
				for _, p := range rep.Problems {
					_, _ = fmt.Fprintln(out, "problem:", p)
				}
				return fmt.Errorf("migrate-from: verification failed with %d problem(s)", len(rep.Problems))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&oldDB, "old-db", "", "path of a copy of the old database, made with VACUUM INTO (or .backup) (required)")
	cmd.Flags().StringVar(&phase, "phase", "", "bulk (before the window) or final (in the window; marks the database complete); required unless --verify")
	cmd.Flags().BoolVar(&verifyOnly, "verify", false, "compare this database with the copy and write nothing")
	cmd.Flags().BoolVar(&precheck, "precheck", false, "run the cutover pre-checks on the copy, report, and change nothing")
	cmd.Flags().StringVar(&reportPath, "report", "", "write the JSON report here (mode 0600)")
	cmd.Flags().IntVar(&batch, "batch", 2000, "rows per key range (key width for tables ranged by an integer key)")
	cmd.Flags().IntVar(&workers, "workers", 0, "parallel range workers for tables ranged by an integer key (0: the CPUs, at most 8)")
	cmd.Flags().StringVar(&targetCheck, "target-check", "full", "check the result after the copy: full (integrity_check), quick (quick_check) or none (the range digests prove the content)")
	cmd.Flags().StringVar(&family, "family", "all", "the table families to copy: a (class A and messages), b (events and telemetry), c (messaging, the delivery library, sessions) or all")
	cmd.Flags().StringVar(&sourceCheck, "source-check", "full", "check the source copy first: full (integrity_check), quick (quick_check) or none (already checked when the copy was made)")
	_ = cmd.MarkFlagRequired("old-db")
	return cmd
}
