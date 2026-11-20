package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/spf13/cobra"
)

func init() { catalogCmd.AddCommand(newCatalogPlanCommand()) }

func newCatalogPlanCommand() *cobra.Command {
	var apply bool
	command := &cobra.Command{
		Use:   "plan <bundle.json>",
		Short: "Plan a catalog bundle (use - to read JSON from stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input := cmd.InOrStdin()
			if args[0] != "-" {
				file, err := os.Open(args[0])
				if err != nil {
					return err
				}
				defer file.Close() //nolint:errcheck
				input = file
			}
			var req api.CatalogPlanRequest
			if err := json.NewDecoder(input).Decode(&req); err != nil {
				return fmt.Errorf("decode bundle: %w", err)
			}
			req.Apply = req.Apply || apply
			h, err := newDaemonClient(catalogPath)
			if err != nil {
				return err
			}
			plan, err := h.CatalogApply().Plan(cmd.Context(), req)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(plan)
		},
	}
	command.Flags().BoolVar(&apply, "apply", false, "Immediately apply; requires daemon TETHER_CATALOG_APPROVAL=off")
	return command
}
