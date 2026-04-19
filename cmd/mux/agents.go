package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/app"
)

var agentsCmd = &cobra.Command{
	Use:   "agents",
	Short: "Agent commands",
}

var agentsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured agents",
	RunE: func(cmd *cobra.Command, args []string) error {
		svc, err := app.New(catalogPath)
		if err != nil {
			return err
		}
		defer func() { _ = svc.Close() }()
		for _, a := range svc.ListAgents() {
			fmt.Printf("%-20s  %s\n", a.ID, a.Name)
		}
		return nil
	},
}

func init() {
	agentsCmd.AddCommand(agentsListCmd)
}
