package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/bootgen"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
)

var bootCmd = &cobra.Command{
	Use:   "boot",
	Short: "Boot prompt commands",
}

var generateBootCmd = &cobra.Command{
	Use:   "generate-boot <profile_id>",
	Short: "Generate a boot prompt and write it to stdout",
	Long: `Assemble a boot prompt from the named profile and write it to stdout.

  mux generate-boot nanite.backend.main | claude --dangerously-skip-permissions

Run 'mux list-boot-profiles' to see available profiles.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := loadBootProfile(args[0])
		if err != nil {
			return err
		}
		return bootgen.Generate(cmd.Context(), p, config.Expand(catalogPath), os.Stdout)
	},
}

var listBootProfilesCmd = &cobra.Command{
	Use:   "list-boot-profiles",
	Short: "List available boot profiles",
	RunE: func(cmd *cobra.Command, args []string) error {
		profilesDir := filepath.Join(config.Expand(catalogPath), "boot-profiles")
		profiles, err := bootgen.LoadProfiles(profilesDir)
		if err != nil {
			return fmt.Errorf("load boot profiles: %w", err)
		}
		if len(profiles) == 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "no boot profiles found in %s\n", profilesDir)
			return nil
		}
		fmt.Printf("%-40s  %-30s  %s\n", "ID", "DISPLAY NAME", "LAUNCH")
		for id, p := range profiles {
			name := p.DisplayName
			if name == "" {
				name = "—"
			}
			launch := p.Launch
			if launch == "" {
				launch = "—"
			}
			fmt.Printf("%-40s  %-30s  %s\n", id, name, launch)
		}
		return nil
	},
}

var bootLaunchCmd = &cobra.Command{
	Use:   "boot <profile_id>",
	Short: "Generate boot prompt, launch a session, and attach",
	Long: `Generate a dynamic boot prompt from the named profile, create a session
using the profile's configured launch ID, and attach to it in the current
terminal. The generated prompt replaces the catalog's static boot fragments.

  mux boot nanite.backend.main

The profile must have a 'launch:' field pointing to a catalog launch ID.
Run 'mux list-boot-profiles' to see which profiles support booting.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		profileID := args[0]

		p, err := loadBootProfile(profileID)
		if err != nil {
			return err
		}
		if p.Launch == "" {
			return fmt.Errorf("profile %q has no 'launch:' field — add one pointing to a catalog launch ID", profileID)
		}

		fmt.Fprintf(cmd.ErrOrStderr(), "generating boot prompt for %s...\n", profileID)
		var buf bytes.Buffer
		if err := bootgen.Generate(cmd.Context(), p, config.Expand(catalogPath), &buf); err != nil {
			return fmt.Errorf("generate boot prompt: %w", err)
		}
		bootPrompt := buf.String()

		cfg, err := loadDaemonConfig(catalogPath)
		if err != nil {
			return err
		}
		inner := client.New(cfg.ListenAddr)

		fmt.Fprintf(cmd.ErrOrStderr(), "creating session with launch %q...\n", p.Launch)
		created, err := inner.CreateSessionWithBootPrompt(context.Background(), p.Launch, bootPrompt)
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}

		fmt.Fprintf(cmd.ErrOrStderr(), "launching %s...\n", created.ID)
		if _, err := inner.LaunchSession(context.Background(), created.ID); err != nil {
			return fmt.Errorf("launch session: %w", err)
		}

		fmt.Fprintf(cmd.ErrOrStderr(), "attaching — Ctrl+C to detach\n")
		return inner.AttachSession(cmd.Context(), created.ID, os.Stdout, 0)
	},
}

// loadBootProfile is a shared helper used by generate-boot and boot commands.
func loadBootProfile(profileID string) (bootgen.Profile, error) {
	profilesDir := filepath.Join(config.Expand(catalogPath), "boot-profiles")
	profiles, err := bootgen.LoadProfiles(profilesDir)
	if err != nil {
		return bootgen.Profile{}, fmt.Errorf("load boot profiles: %w", err)
	}
	p, ok := profiles[profileID]
	if !ok {
		available := make([]string, 0, len(profiles))
		for id := range profiles {
			available = append(available, id)
		}
		if len(available) == 0 {
			return bootgen.Profile{}, fmt.Errorf("no boot profiles found in %s", profilesDir)
		}
		return bootgen.Profile{}, fmt.Errorf("boot profile %q not found\n\nAvailable:\n  %s",
			profileID, strings.Join(available, "\n  "))
	}
	return p, nil
}

// expandCatalogPath returns the absolute catalog root with ~ expanded.
func expandCatalogPath() string { return config.Expand(catalogPath) }

func init() {
	bootCmd.AddCommand(generateBootCmd, listBootProfilesCmd)
	// Alias: mux boot <profile_id> at top level for one-command flow
	bootLaunchCmd.Use = "boot <profile_id>"
}
