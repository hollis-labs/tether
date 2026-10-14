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
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\tconnected=%t\tsessions=%d\torphaned_hosts=%d\t%s\n", w.Label, w.Platform, w.Connected, w.Sessions, w.Orphaned, w.LastSeen)
			for _, h := range w.Hosts {
				fmt.Fprintf(cmd.OutOrStdout(), "  host %s\tsession=%s\tstate=%s\tage=%ds\thost_alive=%t\tchild_alive=%t\torphaned=%t\n", h.Dir, h.Session, h.State, h.AgeSec, h.HostAlive, h.ChildAlive, h.Orphaned)
			}
		}
		return nil
	}})
	workers := rootCmd.Commands()
	for _, c := range workers {
		if c.Name() == "workers" {
			c.Flags().Bool("json", false, "JSON output")
			var remove bool
			grant := &cobra.Command{Use: "grant <worker-id> <urn>", Short: "Grant an Outpost permission to bind an existing agent URN", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
				client, err := localDeviceAdminClient()
				if err != nil {
					return err
				}
				var result any
				if err = client.call(cmd.Context(), http.MethodPost, "/workers/grants", map[string]any{"worker": args[0], "urn": args[1], "remove": remove}, &result); err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}}
			grant.Flags().BoolVar(&remove, "remove", false, "remove grant and stop its live binding")
			c.AddCommand(grant)
			c.AddCommand(&cobra.Command{Use: "reap <worker>", Short: "Terminate a worker's orphaned shim-hosts (live host, no agent child) now", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
				client, err := localDeviceAdminClient()
				if err != nil {
					return err
				}
				var result any
				if err = client.call(cmd.Context(), http.MethodPost, "/workers/reap", map[string]any{"worker": args[0]}, &result); err != nil {
					return err
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}})
		}
	}
}
