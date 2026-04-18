package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/app"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Session commands",
}

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sessions",
	RunE: func(cmd *cobra.Command, args []string) error {
		svc, err := app.New(catalogPath)
		if err != nil {
			return err
		}
		defer svc.Close()
		rows, err := svc.ListSessions()
		if err != nil {
			return err
		}
		fmt.Printf("%-36s  %-12s  %-12s  %-12s  %s\n", "ID", "STATE", "PROJECT", "AGENT", "CREATED")
		for _, r := range rows {
			fmt.Printf("%-36s  %-12s  %-12s  %-12s  %s\n", r.ID, r.State, r.ProjectID, r.AgentID, r.CreatedAt)
		}
		return nil
	},
}

var sessionsGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Show session detail",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		svc, err := app.New(catalogPath)
		if err != nil {
			return err
		}
		defer svc.Close()
		r, err := svc.GetSession(args[0])
		if err != nil {
			return err
		}
		fmt.Printf("id:         %s\nstate:      %s\nlaunch:     %s\nproject:    %s\nagent:      %s\nprovider:   %s\nworkspace:  %s\ncreated_at: %s\nupdated_at: %s\n",
			r.ID, r.State, r.LaunchID, r.ProjectID, r.AgentID, r.ProviderID, r.Workspace, r.CreatedAt, r.UpdatedAt)
		return nil
	},
}

var sessionsTailCmd = &cobra.Command{
	Use:   "tail <id>",
	Short: "Tail the session log",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		svc, err := app.New(catalogPath)
		if err != nil {
			return err
		}
		defer svc.Close()
		r, err := svc.GetSession(args[0])
		if err != nil {
			return err
		}
		f, err := os.Open(r.Workspace + "/logs/session.log")
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(os.Stdout, f)
		return err
	},
}

func init() {
	sessionsCmd.AddCommand(sessionsListCmd, sessionsGetCmd, sessionsTailCmd)
}
