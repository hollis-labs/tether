package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/client"
)

var (
	launchID           string
	launchWait         bool
	launchAgentFile    string
	launchAgentInline  string
	launchBootProfile  string
	launchOverride     string
	launchBootPromptOR string
)

var launchCmd = &cobra.Command{
	Use:   "launch",
	Short: "Launch a session from a launch profile",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return err
		}

		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		var res api.LaunchResponse
		if launchAgentFile != "" || launchAgentInline != "" || launchBootProfile != "" || launchOverride != "" || launchBootPromptOR != "" {
			res, err = c.LaunchWithInput(ctx, api.LaunchRequest{
				Launch:          launchID,
				BootPrompt:      launchBootPromptOR,
				AgentFile:       launchAgentFile,
				AgentInline:     launchAgentInline,
				BootProfileFile: launchBootProfile,
				Override:        launchOverride,
			})
		} else {
			res, err = c.Launch(ctx, launchID)
		}
		if err != nil {
			if errors.Is(err, client.ErrDaemonUnreachable) {
				return fmt.Errorf("agent-mux daemon is not running; run `mux daemon start` first")
			}
			return err
		}
		attachCmd := "mux"
		if bin, err := os.Executable(); err == nil && bin != "" {
			attachCmd = filepath.Base(bin)
		}
		fmt.Fprintf(os.Stderr, "session:   %s\nworkspace: %s\nattach:    %s sessions attach %s\n",
			res.ID, res.Workspace, attachCmd, res.ID)

		if launchWait {
			code, err := c.WaitSession(ctx, res.ID)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "exit: %d\n", code)
			if code != 0 {
				os.Exit(code)
			}
		}
		return nil
	},
}

func init() {
	launchCmd.Flags().StringVar(&launchID, "launch", "", "launch ID (required)")
	launchCmd.Flags().BoolVar(&launchWait, "wait", false, "wait for process to exit")
	launchCmd.Flags().StringVar(&launchAgentFile, "agent-file", "", "v005-08: path to agent YAML; field-merged over the catalog agent")
	launchCmd.Flags().StringVar(&launchAgentInline, "agent-inline", "", "v005-08: inline JSON agent definition (highest precedence)")
	launchCmd.Flags().StringVar(&launchBootProfile, "boot-profile", "", "v005-08: path to bootgen boot-profile YAML (carries MCP allowlist)")
	launchCmd.Flags().StringVar(&launchOverride, "override", "", `v005-08: per-launch JSON override, e.g. '{"system_prompt":"...","env":{"K":"V"}}'`)
	launchCmd.Flags().StringVar(&launchBootPromptOR, "boot-prompt", "", "raw boot-prompt override (wins over all composition layers)")
	_ = launchCmd.MarkFlagRequired("launch")
}
