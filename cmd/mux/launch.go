package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/app"
)

var (
	launchID   string
	launchWait bool
)

var launchCmd = &cobra.Command{
	Use:   "launch",
	Short: "Launch a session from a launch profile",
	RunE: func(cmd *cobra.Command, args []string) error {
		svc, err := app.New(catalogPath)
		if err != nil {
			return err
		}
		defer svc.Close()
		launched, err := svc.Launch(launchID, os.Stdout)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "session: %s\nworkspace: %s\nlog: %s\n",
			launched.SessionID, launched.Workspace.Root, launched.Workspace.LogPath)
		if launchWait {
			code, err := launched.Handle.Wait()
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "exit: %d\n", code)
		}
		return nil
	},
}

func init() {
	launchCmd.Flags().StringVar(&launchID, "launch", "", "launch ID (required)")
	launchCmd.Flags().BoolVar(&launchWait, "wait", false, "wait for process to exit")
	_ = launchCmd.MarkFlagRequired("launch")
}
