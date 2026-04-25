package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/client"
)

var (
	launchID   string
	launchWait bool
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

		res, err := c.Launch(ctx, launchID)
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
	_ = launchCmd.MarkFlagRequired("launch")
}
