package main

import (
	"fmt"
	"github.com/hollis-labs/tether/internal/telemetry"
	"github.com/spf13/cobra"
)

// newToolMetricsCmd keeps its selectors local so invocations/tests do not share
// mutable command flags. Formatting remains the CLI's only policy.
func newToolMetricsCmd() *cobra.Command {
	var req telemetry.MetricsRequest
	var jsonOutput bool
	cmd := &cobra.Command{Use: "tool-metrics", Short: "Read durable tool-call counters and latency histograms", Args: cobra.NoArgs}
	cmd.Flags().StringVar(&req.Tool, "tool", "", "exact tool name")
	cmd.Flags().StringVar(&req.Upstream, "upstream", "", "exact upstream origin")
	cmd.Flags().StringVar(&req.Since, "since", "", "inclusive RFC3339 lower bound")
	cmd.Flags().StringVar(&req.Until, "until", "", "exclusive RFC3339 upper bound")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON output")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.ToolMetrics(cmdCtx(cmd), req)
		if err != nil {
			return classifyErr(err)
		}
		if jsonOutput {
			return printJSON(out)
		}
		for _, g := range out.Groups {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\tcalls=%d\tduration_sum_ms=%d\n", g.Tool, g.Upstream, g.Outcome, g.Calls, g.Duration.SumMs)
		}
		if out.Truncated {
			fmt.Fprintln(cmd.OutOrStdout(), "truncated: narrow --tool/--upstream/--since filters")
		}
		return nil
	}
	return cmd
}
func init() { eventsCmd.AddCommand(newToolMetricsCmd()) }
