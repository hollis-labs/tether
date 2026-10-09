package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/hollis-labs/tether/internal/teamimport"
	"github.com/spf13/cobra"
)

func newImportTeamCmd() *cobra.Command {
	var manifestPath, output string
	cmd := &cobra.Command{
		Use:   "import-team",
		Short: "Convert a reviewed Cairn/team baseline into a fresh isolated catalog",
		Long: `Offline, opt-in conversion of prepared Codex Cairn roles and explicit
tmux baseline selections into supported Tether launch inputs. Does not launch
agents, register identities, resolve credentials, install hooks or modify units.
The destination must not exist. See docs/cairn-team-import.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			file, err := os.Open(manifestPath) //nolint:gosec // Explicit operator-selected non-secret manifest, not a network path.
			if err != nil {
				return fmt.Errorf("open baseline manifest: %w", err)
			}
			defer func() { _ = file.Close() }()
			decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
			decoder.DisallowUnknownFields()
			var manifest teamimport.Manifest
			if err := decoder.Decode(&manifest); err != nil {
				// Decoder errors can include untrusted input. Do not echo a
				// accidentally supplied credential-bearing manifest to logs.
				return errors.New("invalid team baseline JSON; consult the manifest schema")
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				return errors.New("team baseline must contain exactly one JSON object")
			}
			if err := teamimport.Write(manifest, output); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "created opt-in catalog; no session launched")
			return err
		},
	}
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "reviewed non-secret team baseline JSON")
	cmd.Flags().StringVar(&output, "output", "", "absolute fresh owned catalog directory")
	_ = cmd.MarkFlagRequired("manifest")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

func init() { rootCmd.AddCommand(newImportTeamCmd()) }
