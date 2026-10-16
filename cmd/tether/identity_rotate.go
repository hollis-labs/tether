package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/spf13/cobra"
)

func identityRotateOperatorCmd() *cobra.Command {
	var by, evidence string
	cmd := &cobra.Command{
		Use:   "rotate-operator --evidence TEXT",
		Short: "Replace the operator credential: revoke the old one, mint a new one and swap run/operator.token atomically",
		Long: "Revokes every active operator credential in the state database, mints a new one and replaces run/operator.token (written to a temporary 0600 file and renamed into place).\n" +
			"The daemon checks each request against the database, so a RUNNING daemon accepts the new credential and rejects the old one at once; no restart is needed. If the database is locked the command fails with a clear error: retry, or stop the daemon first.\n" +
			"A failure leaves the old database row and the old token file valid. It also repairs a missing, loose or mismatched token file, which daemon startup refuses to do (\"explicit recovery required\").\n" +
			"Nothing secret is printed; the new value only goes to the token file. The rotation is appended to run/identity-flips.jsonl as a hash-chained record carrying short hash prefixes only.\n" +
			"After rotating: restart anything that read the file once at start (long-lived MCP proxies, scripts), and replace any copy of the old credential on other machines.\n" +
			"Refuses inside a Tether-launched session. --by is an unauthenticated label.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseInsideSession(); err != nil {
				return err
			}
			if strings.TrimSpace(evidence) == "" {
				return fmt.Errorf("--evidence is required (task id, owner GO reference)")
			}
			root := config.Expand(catalogPath)
			cat, err := config.Load(root)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			dbPath := config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths)
			if dbPath == "" {
				return fmt.Errorf("state_db not configured in catalog")
			}
			tokenPath := filepath.Join(filepath.Dir(root), "run", "operator.token")
			return runRotateOperator(cmd.Context(), cmd.OutOrStdout(), root, dbPath, tokenPath, by, evidence)
		},
	}
	cmd.Flags().StringVar(&by, "by", "", "label recorded as who rotated (default: OS user; not authenticated)")
	cmd.Flags().StringVar(&evidence, "evidence", "", "task id and owner GO reference (required)")
	return cmd
}

func runRotateOperator(ctx context.Context, w io.Writer, catalogRoot, dbPath, tokenPath, by, evidence string) error {
	if by == "" {
		if u, err := user.Current(); err == nil {
			by = u.Username
		} else {
			by = "unknown"
		}
	}
	// Refuse before touching anything if the audit log cannot be extended.
	if _, _, err := verifyFlipLog(flipLogPath(catalogRoot)); err != nil {
		return fmt.Errorf("refusing to rotate: %w", err)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open state db: %w", err)
	}
	defer func() { _ = db.Close() }()
	res, err := identity.NewStore(db.DB()).RotateOperator(ctx, tokenPath)
	if err != nil {
		if errors.Is(err, identity.ErrOperatorRotationBusy) {
			return fmt.Errorf("%w (nothing was changed)", err)
		}
		return fmt.Errorf("rotate operator credential (the old credential and token file are unchanged): %w", err)
	}
	rec := &flipRecord{
		At: time.Now().UTC().Format(time.RFC3339), Action: "rotate-operator", By: by, Point: "operator",
		From: res.OldHashPrefix, To: res.NewHashPrefix, Evidence: evidence, Catalog: catalogRoot,
	}
	if err := appendFlip(flipLogPath(catalogRoot), rec); err != nil {
		fmt.Fprintf(w, "operator credential rotated (new %s), BUT the audit record could not be written: %v\n", res.NewHashPrefix, err)
		return fmt.Errorf("rotation done, audit record failed: %w", err)
	}
	state := "old file verified"
	if !res.OldFileValid {
		state = "old token file was missing or invalid (recovery)"
	}
	fmt.Fprintf(w, "operator credential rotated: revoked %d, new %s (%s); wrote %s (0600)\n", res.Revoked, res.NewHashPrefix, state, tokenPath)
	fmt.Fprintln(w, "a running daemon already enforces the new credential; restart long-lived readers and replace other copies of the old one")
	return nil
}
