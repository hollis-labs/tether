package main

import (
	"encoding/json"
	"fmt"

	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/app"
)

var resolveLaunchID string

var resolveCmd = &cobra.Command{
	Use:   "resolve",
	Short: "Resolve a launch plan and print it as JSON",
	RunE: func(cmd *cobra.Command, args []string) error {
		svc, err := app.New(catalogPath)
		if err != nil {
			return err
		}
		defer func() { _ = svc.Close() }()

		// S5 toggle: when the launch engine is "spec", this dry-run
		// inspector prints the Spec-resolved agentlaunch.LaunchPlan — the
		// exact plan that engine feeds launcher.Compile. The default
		// ("catalog") prints the legacy launch.Plan unchanged.
		var payload any
		if svc.LaunchEngineIsSpec() {
			payload, err = svc.SpecResolveLaunchPlan(cmd.Context(), resolveLaunchID, agentlaunch.PolicyCollect)
		} else {
			payload, err = svc.Resolve(resolveLaunchID)
		}
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	},
}

func init() {
	resolveCmd.Flags().StringVar(&resolveLaunchID, "launch", "", "launch ID (required)")
	_ = resolveCmd.MarkFlagRequired("launch")
}
