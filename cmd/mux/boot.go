package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/bootgen"
	"github.com/chrispian/agent-mux/internal/config"
)

// expandCatalogPath returns the absolute catalog root with ~ expanded.
func expandCatalogPath() string { return config.Expand(catalogPath) }

var bootCmd = &cobra.Command{
	Use:   "boot",
	Short: "Boot prompt commands",
}

var generateBootCmd = &cobra.Command{
	Use:   "generate-boot <profile_id>",
	Short: "Generate a boot prompt and write it to stdout",
	Long: `Assemble a boot prompt from the named profile and write it to stdout.

The profile is a YAML file at <catalog>/boot-profiles/<id>.yaml that specifies
how to populate each slot (static files, shell commands, HTTP endpoints).

Pipe directly to your CLI tool:

  mux generate-boot nanite.backend.main | claude --dangerously-skip-permissions

Or capture it:

  BOOT=$(mux generate-boot nanite.backend.main)
  echo "$BOOT" | claude -p "$BOOT"

Profile IDs use the convention <project>.<role>.<profile>, e.g.:

  nanite.backend.main
  agent-mux.engineer.main
  agent-ops.steward.main

Run 'mux list-boot-profiles' to see available profiles.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		profileID := args[0]

		profilesDir := filepath.Join(expandCatalogPath(), "boot-profiles")
		profiles, err := bootgen.LoadProfiles(profilesDir)
		if err != nil {
			return fmt.Errorf("load boot profiles: %w", err)
		}

		p, ok := profiles[profileID]
		if !ok {
			available := make([]string, 0, len(profiles))
			for id := range profiles {
				available = append(available, id)
			}
			if len(available) == 0 {
				return fmt.Errorf("no boot profiles found in %s\n\nCreate a profile YAML at %s/<id>.yaml",
					profilesDir, profilesDir)
			}
			return fmt.Errorf("boot profile %q not found\n\nAvailable profiles:\n  %s",
				profileID, strings.Join(available, "\n  "))
		}

		return bootgen.Generate(cmd.Context(), p, expandCatalogPath(), os.Stdout)
	},
}

var listBootProfilesCmd = &cobra.Command{
	Use:   "list-boot-profiles",
	Short: "List available boot profiles",
	RunE: func(cmd *cobra.Command, args []string) error {
		profilesDir := filepath.Join(expandCatalogPath(), "boot-profiles")
		profiles, err := bootgen.LoadProfiles(profilesDir)
		if err != nil {
			return fmt.Errorf("load boot profiles: %w", err)
		}
		if len(profiles) == 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "no boot profiles found in %s\n", profilesDir)
			return nil
		}
		fmt.Printf("%-40s  %s\n", "ID", "DISPLAY NAME")
		for id, p := range profiles {
			name := p.DisplayName
			if name == "" {
				name = "—"
			}
			fmt.Printf("%-40s  %s\n", id, name)
		}
		return nil
	},
}

func init() {
	bootCmd.AddCommand(generateBootCmd, listBootProfilesCmd)
}
