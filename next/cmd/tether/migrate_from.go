package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/substrate/modules/database"

	"github.com/hollis-labs/tether/next/internal/migrate"
)

// migrateFromCmd is `tether migrate-from` (plan section 7.2): copy an old-layout database into this data
// directory's database and prove the copy. It never opens the live file: the operator gives a copy (the
// daily backup's, or one taken with the old hub stopped).
func (t *tether) migrateFromCmd(ctx context.Context) *cobra.Command {
	var oldDB, phase, reportPath string
	var batch int
	cmd := &cobra.Command{
		Use:   "migrate-from",
		Short: "Copy an old Tether database (a copy, never the live file) into this one and verify it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkSource(oldDB); err != nil {
				return err
			}
			src, err := migrate.OpenSource(oldDB)
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()
			out := cmd.OutOrStdout()
			e := migrate.New(src, oldDB, database.From(t.app))
			rep, err := e.Run(ctx, migrate.Options{
				Phase: migrate.Phase(phase),
				Batch: batch,
				Progress: func(format string, args ...any) {
					_, _ = fmt.Fprintf(out, format+"\n", args...)
				},
			})
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
			_, _ = fmt.Fprintf(out, "mark %s; copy %.1fs, verify %.1fs; source unchanged: %v\n", rep.Mark, rep.CopySeconds, rep.VerifySeconds, rep.SourceUnchanged)
			if !rep.OK() {
				for _, p := range rep.Problems {
					_, _ = fmt.Fprintln(out, "problem:", p)
				}
				return fmt.Errorf("migrate-from: verification failed with %d problem(s)", len(rep.Problems))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&oldDB, "old-db", "", "path of a copy of the old database (required)")
	cmd.Flags().StringVar(&phase, "phase", string(migrate.PhaseFinal), "bulk (before the window) or final (in the window; marks the database complete)")
	cmd.Flags().StringVar(&reportPath, "report", "", "write the JSON report here (mode 0600)")
	cmd.Flags().IntVar(&batch, "batch", 2000, "rows per key range")
	_ = cmd.MarkFlagRequired("old-db")
	return cmd
}

// checkSource refuses a source that looks live: a WAL or shared-memory file beside it means a process has
// it open in WAL mode. A copy taken with VACUUM INTO or from a stopped hub has neither.
func checkSource(path string) error {
	if path == "" {
		return errors.New("--old-db is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(abs + side); err == nil {
			return fmt.Errorf("%s has a %s file beside it, so it looks live: migrate a copy (the daily backup's, or one taken with the old hub stopped)", abs, side)
		}
	}
	return nil
}
