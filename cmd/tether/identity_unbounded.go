package main

import (
	"context"
	"fmt"
	"io"
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
