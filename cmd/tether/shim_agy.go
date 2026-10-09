//go:build !windows

package main

import (
	"encoding/json"
	"errors"

	"github.com/hollis-labs/tether/internal/shimagy"
	"github.com/spf13/cobra"
)

func init() {
	var encoded string
	cmd := &cobra.Command{Use: "shim-agy", Hidden: true, SilenceUsage: true, RunE: func(cmd *cobra.Command, _ []string) error {
		var cfg shimagy.Config
		if len(encoded) > shimagy.MaxConfig || json.Unmarshal([]byte(encoded), &cfg) != nil {
			return errors.New("AGY worker configuration refused")
		}
		return shimagy.Run(cmd.Context(), cfg, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
	}}
	cmd.Flags().StringVar(&encoded, "config", "", "resolved internal AGY launch")
	rootCmd.AddCommand(cmd)
}
