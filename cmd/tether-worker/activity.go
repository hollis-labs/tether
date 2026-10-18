package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hollis-labs/tether/internal/pulse"
	"github.com/spf13/cobra"
)

func activityCommand() *cobra.Command {
	return &cobra.Command{
		Use: "activity", Short: "Inspect the local manual-agent activity batch without sending it", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			batch, err := pulse.New().Scan(ctx, time.Now())
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(batch)
		},
	}
}
