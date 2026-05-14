package main

import (
	"encoding/json"
	"fmt"

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
		plan, err := svc.Resolve(resolveLaunchID)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(plan, "", "  ")
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
