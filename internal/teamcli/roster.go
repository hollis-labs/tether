package teamcli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/teamsvc"
	"github.com/spf13/cobra"
)

func invalidRosterCommand() error { return &client.TeamError{Code: "invalid_request", Status: 400} }

func addRosterCommands(root *cobra.Command, factory func() (*client.Client, error)) {
	var after string
	var limit int
	ls := &cobra.Command{Use: "ls [RUN]", Short: "List your retained team rosters", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		req := teamsvc.RosterRequest{After: after, Limit: limit}
		if len(args) == 1 {
			req.RunID = args[0]
		}
		if teamsvc.ValidateRosterRequest(req) != nil {
			return invalidRosterCommand()
		}
		c, err := factory()
		if err != nil {
			return &client.TeamError{Code: "unavailable", Status: 503}
		}
		result, err := c.TeamRoster(cmd.Context(), req)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	ls.Flags().StringVar(&after, "after", "", "Continue after this run ID")
	ls.Flags().IntVar(&limit, "limit", 50, "Maximum runs (1–100)")
	root.AddCommand(ls)

	var stopKey string
	var all, cascade bool
	stop := &cobra.Command{Use: "stop RUN [MEMBER]", Short: "Stop a member, or explicitly dissolve a run", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		if all == (len(args) == 2) || all && cascade {
			return invalidRosterCommand()
		}
		req := api.TeamRequest{RunID: args[0], Cascade: cascade}
		verb := "dissolve"
		if !all {
			verb, req.MemberID = "cancel", args[1]
		}
		return rosterMutation(cmd, factory, verb, stopKey, req)
	}}
	stop.Flags().StringVar(&stopKey, "key", "", "Caller-scoped idempotency key (required)")
	stop.Flags().BoolVar(&all, "all", false, "Dissolve this entire run")
	stop.Flags().BoolVar(&cascade, "cascade", false, "Include this member's descendants")
	root.AddCommand(stop)

	var msgKey, bodyFile string
	msg := &cobra.Command{Use: "msg RUN ADDRESS [BODY]", Short: "Send through the team's authorized route", Args: cobra.RangeArgs(2, 3), RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 3 && bodyFile != "" {
			return invalidRosterCommand()
		}
		body := ""
		if len(args) == 3 {
			body = args[2]
		}
		if bodyFile != "" {
			content, err := rosterInput(cmd, bodyFile, teamsvc.MaxBodyBytes)
			if err != nil {
				return err
			}
			body = string(content)
		}
		return rosterMutation(cmd, factory, "address", msgKey, api.TeamRequest{RunID: args[0], Address: args[1], Body: body})
	}}
	msg.Flags().StringVar(&msgKey, "key", "", "Caller-scoped idempotency key (required)")
	msg.Flags().StringVar(&bodyFile, "body-file", "", "Read message from a file, or - for stdin")
	root.AddCommand(msg)

	var bootKey, definitionFile string
	boot := &cobra.Command{Use: "boot", Short: "Form a run from an authored team definition", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if definitionFile == "" {
			return invalidRosterCommand()
		}
		content, err := rosterInput(cmd, definitionFile, teamsvc.MaxTeamBytes)
		if err != nil {
			return err
		}
		// The existing strict request decoder remains the authority for definition
		// shape and limits. Workspace mission/brief are non-secret context only.
		var body bytes.Buffer
		body.WriteString("{\"team\":")
		body.Write(content)
		body.WriteByte('}')
		req, err := api.DecodeTeamRequest("form", bootKey, &body)
		if err != nil {
			return invalidRosterCommand()
		}
		return rosterMutation(cmd, factory, "form", bootKey, req)
	}}
	boot.Flags().StringVar(&bootKey, "key", "", "Caller-scoped idempotency key (required)")
	boot.Flags().StringVar(&definitionFile, "definition", "", "Team JSON file, or - for stdin")
	root.AddCommand(boot)
}

func rosterInput(cmd *cobra.Command, path string, bound int) ([]byte, error) {
	reader := cmd.InOrStdin()
	if path != "-" {
		file, err := os.Open(path) //nolint:gosec // Explicit operator-selected input; no daemon state is opened.
		if err != nil {
			return nil, invalidRosterCommand()
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	content, err := io.ReadAll(io.LimitReader(reader, int64(bound)+1))
	if err != nil || len(content) > bound {
		return nil, invalidRosterCommand()
	}
	return content, nil
}

func rosterMutation(cmd *cobra.Command, factory func() (*client.Client, error), verb, key string, req api.TeamRequest) error {
	content, err := json.Marshal(req)
	if err != nil {
		return invalidRosterCommand()
	}
	if _, err = api.DecodeTeamRequest(verb, key, strings.NewReader(string(content))); err != nil {
		return invalidRosterCommand()
	}
	c, err := factory()
	if err != nil {
		return &client.TeamError{Code: "unavailable", Status: 503}
	}
	result, err := c.Team(cmd.Context(), verb, key, req)
	if err != nil {
		return err
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
}
