package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/store"
)

// tether migrations (CW-20261115-0046): what this binary would migrate, and
// applying it as its own step, for the release tooling. A release can ask the
// NEW binary which migrations it will run against the live state database
// before swapping it in, and a long one can run before the swap instead of
// inside the daemon's start.

var migrationsCmd = &cobra.Command{
	Use:   "migrations",
	Short: "Inspect or apply this binary's state-database migrations (release tooling)",
}

var migrationsStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "List the migrations this binary would apply to the state database, and any in progress",
	Long: `tether migrations status reads the state database read-only and reports the
migrations this binary would apply (pending) and, if a daemon is migrating it
right now, which one and since when (in_progress). It never writes.

Exit code 0; --json prints {state_db, exists, pending, in_progress}.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		dbPath, err := migrationsStateDB(cmd)
		if err != nil {
			return err
		}
		asJSON, _ := cmd.Flags().GetBool("json")
		return writeMigrationsStatus(cmd.OutOrStdout(), dbPath, asJSON)
	},
}

var migrationsApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply this binary's pending migrations to the state database, logging each",
	Long: `tether migrations apply opens the state database and applies the migrations
this binary carries that it lacks, logging each one with its elapsed time, as
the daemon would at start. Use it to run a long migration as its own release
step before the binary swap.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		dbPath, err := migrationsStateDB(cmd)
		if err != nil {
			return err
		}
		start := time.Now()
		s, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("migrate %s: %w", dbPath, err)
		}
		if err = s.Close(); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "migrations: %s is current (%s)\n", dbPath, time.Since(start).Round(time.Millisecond))
		return nil
	},
}

func init() {
	for _, c := range []*cobra.Command{migrationsStatusCmd, migrationsApplyCmd} {
		c.Flags().String("state-db", "", "state database path (default: the catalog's defaults.state_db)")
	}
	migrationsStatusCmd.Flags().Bool("json", false, "print JSON")
	migrationsCmd.AddCommand(migrationsStatusCmd, migrationsApplyCmd)
	rootCmd.AddCommand(migrationsCmd)
}

func migrationsStateDB(cmd *cobra.Command) (string, error) {
	if p, _ := cmd.Flags().GetString("state-db"); p != "" {
		return config.Expand(p), nil
	}
	cat, err := config.LoadLayered(catalogPath)
	if err != nil {
		return "", err
	}
	p := config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths)
	if p == "" {
		return "", fmt.Errorf("state_db path not configured: pass --state-db or set catalog.defaults.state_db")
	}
	return p, nil
}

// MigrationsStatus is tether migrations status --json.
type MigrationsStatus struct {
	StateDB    string                   `json:"state_db"`
	Exists     bool                     `json:"exists"`
	Pending    []int                    `json:"pending"`
	InProgress *store.MigrationProgress `json:"in_progress,omitempty"`
	// InProgressSeconds is how long the in-progress migration has run.
	InProgressSeconds int64 `json:"in_progress_seconds,omitempty"`
}

func writeMigrationsStatus(w io.Writer, dbPath string, asJSON bool) error {
	st := MigrationsStatus{StateDB: dbPath, Pending: []int{}}
	if _, err := os.Stat(dbPath); err == nil {
		st.Exists = true
		s, err := store.OpenReadOnly(dbPath)
		if err != nil {
			return fmt.Errorf("open %s read-only: %w", dbPath, err)
		}
		pending, err := store.PendingMigrations(s.DB())
		_ = s.Close()
		if err != nil {
			return err
		}
		if pending != nil {
			st.Pending = pending
		}
	}
	if p, ok, err := store.ReadMigrationProgress(dbPath); err != nil {
		return err
	} else if ok {
		st.InProgress, st.InProgressSeconds = &p, int64(time.Since(p.StartedAt)/time.Second)
	}
	if asJSON {
		return json.NewEncoder(w).Encode(st)
	}
	_, _ = fmt.Fprintf(w, "state db: %s\npending:  %v\n", dbPath, st.Pending)
	if st.InProgress != nil {
		_, _ = fmt.Fprintf(w, "running:  %04d_%s for %ds (pid %d)\n", st.InProgress.Version, st.InProgress.Name, st.InProgressSeconds, st.InProgress.PID)
	}
	return nil
}
