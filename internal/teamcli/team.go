package teamcli

import (
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/teamsvc"
	"github.com/spf13/cobra"
)

// NewCommand constructs the thin daemon-backed team command tree.
func NewCommand(factory func() (*client.Client, error), enabled bool) *cobra.Command {
	if !enabled {
		return nil
	}
	cmd := &cobra.Command{Use: "team", Short: "Manage teams through the daemon", SilenceUsage: true, SilenceErrors: true}
	for _, verb := range api.TeamVerbs() {
		var key, request, file string
		sub := &cobra.Command{Use: verb, Short: "Team " + verb, Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true, RunE: func(cmd *cobra.Command, _ []string) error {
			var reader io.Reader = strings.NewReader(request)
			if file != "" {
				if file == "-" {
					reader = cmd.InOrStdin()
				} else {
					f, err := os.Open(file) //nolint:gosec // The operator explicitly selected this local request file.
					if err != nil {
						return &client.TeamError{Code: "invalid_request", Status: 400}
					}
					defer func() { _ = f.Close() }()
					reader = f
				}
			}
			req, err := api.DecodeTeamRequest(verb, key, reader)
			if err != nil {
				status, detail := api.TeamError(err)
				return &client.TeamError{Code: detail.Code, Status: status}
			}
			c, err := factory()
			if err != nil {
				return &client.TeamError{Code: "unavailable", Status: 503}
			}
			var result teamsvc.Result
			result, err = c.Team(cmd.Context(), verb, key, req)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}}
		sub.Flags().StringVar(&key, "key", "", "Caller-scoped idempotency key (required)")
		sub.Flags().StringVar(&request, "request", "{}", "Verb arguments as JSON (no caller identity fields)")
		sub.Flags().StringVar(&file, "request-file", "", "Read verb JSON from a file, or - for stdin")
		sub.MarkFlagsMutuallyExclusive("request", "request-file")
		cmd.AddCommand(sub)
	}
	return cmd
}
