package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/spf13/cobra"
)

// pathCmd prints Tether's resolved on-disk layout — the go-apppaths roots
// plus the EFFECTIVE storage paths the daemon and CLI will actually use.
//
// For state_db / workspace_root / temp_root it prints what
// config.Resolve{StateDB,WorkspaceRoot,TempRoot} return for the loaded
// catalog, so an operator can see at a glance whether the explicit catalog
// value or the go-apppaths fallback is in effect.
//
// It resolves through config.Load(catalogPath), so the printed values
// reflect the real catalog and any env override (TETHER_DB_PATH,
// TETHER_WORKSPACE, $XDG_*).
func pathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print Tether's resolved on-disk layout (go-apppaths)",
		Long: "Print the data/state/cache/config roots, active workspace, and\n" +
			"main database path Tether resolves via go-apppaths, plus the\n" +
			"effective state_db, workspace_root, and temp_root — showing whether\n" +
			"the explicit catalog value or the go-apppaths fallback is in effect.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, err := config.Load(catalogPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			d := cat.Global.Catalog.Defaults
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			for _, e := range cat.Paths.Describe() {
				fmt.Fprintf(w, "%s\t%s\n", e.Label, e.Value)
			}
			fmt.Fprintf(w, "state-db\t%s\t(%s)\n",
				config.ResolveStateDB(d, cat.Paths), sourceOf(d.StateDB))
			fmt.Fprintf(w, "workspace-root\t%s\t(%s)\n",
				config.ResolveWorkspaceRoot(d, cat.Paths), sourceOf(d.WorkspaceRoot))
			fmt.Fprintf(w, "temp-root\t%s\t(%s)\n",
				config.ResolveTempRoot(d, cat.Paths), sourceOf(d.TempRoot))
			return w.Flush()
		},
	}
}

// sourceOf reports whether a resolved storage path came from the explicit
// catalog value or the go-apppaths fallback.
func sourceOf(catalogValue string) string {
	if catalogValue != "" {
		return "catalog"
	}
	return "go-apppaths fallback"
}
