package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/hollis-labs/go-agent-launch/agentlaunch"
	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/bootexec"
	"github.com/hollis-labs/tether/internal/bootgen"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/workspace"
)

var bootPromptsCmd = &cobra.Command{
	Use:   "boot-prompts",
	Short: "Boot prompt commands",
	Long: `Utilities for listing and generating boot prompts.

For the all-in-one launch flow, use:

  mux boot <profile_id>`,
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

var bootExecCmd = &cobra.Command{
	Use:   "boot-exec <profile_id>",
	Short: "Generate boot prompt and run the Claude CLI directly (Claude TUI only)",
	Long: `Generate a dynamic boot prompt from the named profile, materialize the
provider boot files, and run the underlying Claude CLI directly in the current
terminal. This bypasses Tether session creation, daemon attach, and log replay.

  mux boot-exec torque.engineer.tui

The profile must have a 'launch:' field pointing to a catalog launch ID.

boot-exec supports Claude TUI launch profiles only — it execs into the native
Claude PTY runtime. Profiles whose provider is Codex or Opencode are rejected
with a clear error. Codex and Opencode are fully supported as managed
sessions: use 'mux boot <profile>' or 'mux launch' for those providers. See
docs/adr/0039-boot-exec-claude-only-scope.md.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		profileID := args[0]

		p, profilePath, err := loadBootProfileFile(profileID)
		if err != nil {
			return err
		}
		if p.Launch == "" {
			return fmt.Errorf("profile %q has no 'launch:' field — add one pointing to a catalog launch ID", profileID)
		}

		catalogRoot := expandCatalogPath()
		cat, err := config.LoadLayered(catalogRoot)
		if err != nil {
			return err
		}
		if err := cat.Validate(); err != nil {
			return err
		}
		svc := &app.Service{CatalogRoot: catalogRoot, Catalog: cat}
		plan, err := svc.BuildLaunchPlan(app.CreateSessionInput{
			LaunchID:        p.Launch,
			BootProfileFile: profilePath,
		})
		if err != nil {
			return err
		}
		wsRoot := plan.WriteHome
		if err := workspace.MaterializeWorkRoot(wsRoot, "boot-exec-"+uuid.NewString(), plan); err != nil {
			return err
		}
		defer func() { _ = workspace.RemoveMaterializedWorkRoot(plan) }()
		if err := svc.RefreshBootProfilePrompt(cmd.Context(), plan); err != nil {
			return err
		}

		muxCommand := ""
		if exe, err := os.Executable(); err == nil && exe != "" {
			muxCommand = exe
		}
		muxEnv := muxEnvFromPlan(plan.Env)
		// Explicit catalog temp_root wins; cat.Paths supplies the go-apppaths
		// fallback (CacheDir) only when global.yaml omits the key.
		tempRoot := config.ResolveTempRoot(cat.Global.Catalog.Defaults, cat.Paths)
		if tempRoot != "" {
			tempRoot = filepath.Join(tempRoot, "boot-exec")
		}

		// S5 toggle: when the launch engine is "spec", obtain the
		// agentlaunch.LaunchPlan from the Spec resolver instead of from the
		// catalog walk. Compile/Prepare/Plant inside PrepareClaudeTUI are
		// unchanged. Default ("catalog") leaves specPlan nil.
		var specPlan *agentlaunch.LaunchPlan
		if svc.LaunchEngineIsSpec() {
			resolved, err := svc.SpecResolveLaunchPlan(cmd.Context(), p.Launch, agentlaunch.FrontEndInteractive)
			if err != nil {
				return fmt.Errorf("spec launch engine: resolve %q: %w", p.Launch, err)
			}
			specPlan = &resolved
		}

		prepared, err := bootexec.PrepareClaudeTUI(plan, bootexec.Options{
			BootDirRoot:      tempRoot,
			APIKeyHelperPath: app.ResolveAPIKeyHelperPath(),
			MuxCommand:       muxCommand,
			MuxArgs:          app.MuxMCPArgs(catalogRoot),
			MuxEnv:           muxEnv,
			ParentEnv:        os.Environ(),
			SpecPlan:         specPlan,
		})
		if err != nil {
			return err
		}
		defer func() {
			_ = os.RemoveAll(prepared.BootDir)
			_ = os.RemoveAll(prepared.WorkspaceDir)
		}()

		fmt.Fprintf(cmd.ErrOrStderr(), "boot dir: %s\n", prepared.BootDir)
		fmt.Fprintf(cmd.ErrOrStderr(), "launching %s directly...\n", prepared.Command)
		return runPreparedCLI(cmd, prepared)
	},
}

// loadBootProfile is a shared helper used by generate-boot and boot commands.
func loadBootProfile(profileID string) (bootgen.Profile, error) {
	p, _, err := loadBootProfileFile(profileID)
	return p, err
}

func loadBootProfileFile(profileID string) (bootgen.Profile, string, error) {
	profilesDir := filepath.Join(config.Expand(catalogPath), "boot-profiles")
	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return bootgen.Profile{}, "", fmt.Errorf("no boot profiles found in %s", profilesDir)
		}
		return bootgen.Profile{}, "", fmt.Errorf("load boot profiles: %w", err)
	}
	available := []string{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(profilesDir, e.Name())
		p, err := bootgen.LoadProfile(path)
		if err != nil {
			return bootgen.Profile{}, "", fmt.Errorf("load boot profiles: %w", err)
		}
		available = append(available, p.ID)
		if p.ID == profileID {
			return p, path, nil
		}
	}
	if len(available) == 0 {
		return bootgen.Profile{}, "", fmt.Errorf("no boot profiles found in %s", profilesDir)
	}
	return bootgen.Profile{}, "", fmt.Errorf("boot profile %q not found\n\nAvailable:\n  %s",
		profileID, strings.Join(available, "\n  "))
}

// expandCatalogPath returns the absolute catalog root with ~ expanded.
func expandCatalogPath() string { return config.Expand(catalogPath) }

func muxEnvFromPlan(env map[string]string) []string {
	if env == nil || env["MUX_MCP_SERVERS"] == "" {
		return nil
	}
	return []string{"MUX_MCP_SERVERS=" + env["MUX_MCP_SERVERS"]}
}

func runPreparedCLI(cmd *cobra.Command, prepared *bootexec.Prepared) error {
	child := exec.CommandContext(cmd.Context(), prepared.Command, prepared.Args...) //nolint:gosec // command comes from operator-controlled catalog
	child.Dir = prepared.Dir
	child.Env = prepared.Env
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

func init() {
	bootPromptsCmd.AddCommand(generateBootCmd, listBootProfilesCmd)
	// Alias: mux boot <profile_id> at top level for one-command flow
	bootLaunchCmd.Use = "boot <profile_id>"
}
