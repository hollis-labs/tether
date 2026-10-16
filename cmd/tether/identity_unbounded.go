package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/spf13/cobra"
)

// identityUnboundedCmd lists the live session credentials that nothing bounded
// at launch (CW-20261016-0218). It is read-only; the enforce flip uses the same
// list to decide whether it may proceed.
func identityUnboundedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unbounded",
		Short: "List and count live session credentials that were launched with no bounding authority (anonymous, internal-unattributed, or legacy with an unknown launcher)",
		Long: "Reads the state database directly; changes nothing.\n" +
			"anonymous_external: launched with no verified principal (only possible while identity is observe or off).\n" +
			"internal_unattributed: started by the daemon with no origin session to bound it.\n" +
			"legacy_unknown_launcher: minted before the launcher was recorded (created_by is empty).\n" +
			"Each of these holds the full launch-profile scopes. Switching identity to enforce must not leave them standing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := config.Load(config.Expand(catalogPath))
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			dbPath := config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths)
			if dbPath == "" {
				return fmt.Errorf("state_db not configured in catalog")
			}
			db, err := store.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open state db: %w", err)
			}
			defer func() { _ = db.Close() }()
			_, err = reportUnbounded(cmd.Context(), cmd.OutOrStdout(), identity.NewStore(db.DB()))
			return err
		},
	}
}

// reportUnbounded prints the per-attribution counts and one line per credential,
// and returns the list.
func reportUnbounded(ctx context.Context, w io.Writer, ids *identity.Store) ([]identity.UnboundedSession, error) {
	list, err := ids.UnboundedSessions(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, u := range list {
		counts[u.Attribution]++
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	fmt.Fprintf(w, "%d live unbounded session credential(s)\n", len(list))
	for _, k := range kinds {
		fmt.Fprintf(w, "  %-26s %d\n", k, counts[k])
	}
	for _, u := range list {
		expires := "no expiry"
		if u.ExpiresAt != nil {
			expires = "expires " + u.ExpiresAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s  %s  created %s  %s\n", u.SessionID, u.Attribution, u.CreatedAt.UTC().Format(time.RFC3339), expires)
	}
	return list, nil
}

// enforceGuard stops a switch to enforce from leaving unbounded session
// credentials standing (architect ruling anonymous_worker_launch_scopes,
// item 3): credentials minted while identity was observe or off, with no
// bounding authority, keep the full launch-profile scopes for their whole life,
// so enforcing would otherwise only harden new traffic. With no live unbounded
// credential it does nothing. With some, it prints the same counts and list
// `identity unbounded` prints and refuses, unless revoke is set, in which case it
// revokes exactly that list (their sessions lose authentication; relaunch them
// first to avoid that) and returns how many. It fails closed: a state database
// that exists but cannot be read refuses the flip.
func enforceGuard(ctx context.Context, w io.Writer, catalogRoot string, revoke bool) (int, error) {
	cat, err := config.Load(catalogRoot)
	if err != nil {
		return 0, fmt.Errorf("load config: %w", err)
	}
	dbPath := config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths)
	if dbPath == "" {
		return 0, fmt.Errorf("cannot check for unbounded session credentials: state_db is not configured in the catalog")
	}
	if _, statErr := os.Stat(dbPath); errors.Is(statErr, os.ErrNotExist) {
		return 0, nil // no state yet, so no credential exists
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return 0, fmt.Errorf("cannot check for unbounded session credentials (refusing to enforce): open state db: %w", err)
	}
	defer func() { _ = db.Close() }()
	ids := identity.NewStore(db.DB())
	list, err := ids.UnboundedSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("cannot check for unbounded session credentials (refusing to enforce): %w", err)
	}
	if len(list) == 0 {
		return 0, nil
	}
	if _, err = reportUnbounded(ctx, w, ids); err != nil {
		return 0, err
	}
	if !revoke {
		return 0, fmt.Errorf("refusing to switch to enforce: %d live session credential(s) were launched with no bounding authority and would keep their full scopes (listed above). Relaunch those sessions first, or re-run with --revoke-anonymous to revoke exactly that list (their sessions lose authentication)", len(list))
	}
	n, err := ids.RevokeUnbounded(ctx)
	if err != nil {
		return 0, fmt.Errorf("revoke unbounded session credentials: %w", err)
	}
	fmt.Fprintf(w, "revoked %d unbounded session credential(s)\n", n)
	return n, nil
}
