package main

import (
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/spf13/cobra"
	"net/http"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{Use: "workers", Short: "List hub-connected execution outposts", RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := localDeviceAdminClient()
		if err != nil {
			return err
		}
		var result struct {
			Workers []workerhub.Record `json:"workers"`
		}
		if err = c.call(cmd.Context(), http.MethodGet, "/workers", nil, &result); err != nil {
			return err
		}
		if cmd.Flags().Changed("json") {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}
		for _, w := range result.Workers {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\tconnected=%t\tsessions=%d\t%s\n", w.Label, w.Platform, w.Connected, w.Sessions, w.LastSeen)
		}
		return nil
	}})
	workers := rootCmd.Commands()
	for _, c := range workers {
		if c.Name() == "workers" {
			c.Flags().Bool("json", false, "JSON output")
		}
	}
}
