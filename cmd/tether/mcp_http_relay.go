package main

import (
	"github.com/hollis-labs/tether/internal/gatewaylaunch"
	"github.com/hollis-labs/tether/internal/workerruntime"
	"github.com/spf13/cobra"
	"os"
)

func newMCPHTTPRelayCommand() *cobra.Command {
	var endpoint, path, runPath string
	cmd := &cobra.Command{Use: "mcp-http-relay", Hidden: true, SilenceUsage: true, Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		token, err := gatewaylaunch.ReadRunToken(runPath, os.Getenv("TETHER_RUN_TOKEN"))
		if err != nil {
			return err
		}
		relay := &workerruntime.HTTPRelay{URL: endpoint, TokenFile: path, RunToken: token, Stderr: c.ErrOrStderr()}
		return relay.Run(c.Context(), c.InOrStdin(), c.OutOrStdout())
	}}
	cmd.Flags().StringVar(&endpoint, "url", "", "streamable HTTP gateway endpoint")
	cmd.Flags().StringVar(&path, "token-file", "", "rotating session credential file")
	cmd.Flags().StringVar(&runPath, "run-token-file", "", "protected execution-token handoff")
	return cmd
}
