package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/spf13/cobra"
)

// settingsCmd reads app configuration only; it does not open the state database.
func settingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Show effective event retention from app configuration",
		Long: "Show the effective daemon.events_retention window loaded from the catalog.\n" +
			"The daemon applies catalog changes on restart; this does not inspect its running configuration.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, err := config.LoadLayered(catalogPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			jsonOut, _ := cmd.Flags().GetBool("json")
			return printRetentionSettings(cmd.OutOrStdout(), cat.Global.Daemon.EventsRetention, jsonOut)
		},
	}
	cmd.Flags().Bool("json", false, "print effective app retention as JSON")
	return cmd
}

func retentionMessage(c config.EventsRetentionConfig) string {
	window := c.Window()
	if window == 0 {
		return "catalog: disabled for events, proxy_events, ai_events (enabled: false or days < 1); restart applies changes"
	}
	return fmt.Sprintf("catalog: enabled, %d days for events, proxy_events, ai_events; restart applies changes", window/(24*time.Hour))
}

func printRetentionSettings(out io.Writer, c config.EventsRetentionConfig, jsonOut bool) error {
	if jsonOut {
		window := c.Window()
		value := struct {
			Source    string `json:"source"`
			Retention struct {
				Enabled bool     `json:"enabled"`
				Days    int64    `json:"days"`
				Tables  []string `json:"tables"`
			} `json:"daemon.events_retention"`
		}{Source: "catalog (restart applies changes)"}
		value.Retention.Enabled = window > 0
		value.Retention.Days = int64(window / (24 * time.Hour))
		value.Retention.Tables = []string{"events", "proxy_events", "ai_events"}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(value); err != nil {
			return fmt.Errorf("encode settings: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintf(out, "daemon.events_retention: %s\n", retentionMessage(c)); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}
