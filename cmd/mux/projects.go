package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/app"
)

var projectsCmd = &cobra.Command{
	Use:   "projects",
	Short: "Project commands",
}

var projectsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured projects",
	RunE: func(cmd *cobra.Command, args []string) error {
		// TODO v002-s08: route through daemon via client.ListProjects
		// when the daemon is up (falls back to this filesystem read
		// when unreachable). CLI reshuffle is scoped separately.
		svc, err := app.New(catalogPath)
		if err != nil {
			return err
		}
		defer func() { _ = svc.Close() }()
		for _, p := range svc.ListProjects() {
			fmt.Printf("%-20s  %s\n", p.ID, p.Name)
		}
		return nil
	},
}

func init() {
	projectsCmd.AddCommand(projectsListCmd)
}
