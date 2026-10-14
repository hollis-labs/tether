package main

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/sessionpeek"
)

const peekDefaultLines = sessionpeek.DefaultLines

func runSessionsPeek(ctx context.Context, w io.Writer, c *client.Client, id string, n int) error {
	sess, err := c.GetSession(ctx, id)
	if err != nil {
		return err
	}
	exit := "-"
	if sess.ExitCode != nil {
		exit = strconv.Itoa(*sess.ExitCode)
	}
	fmt.Fprintf(w, "session %s state=%s exit=%s provider=%s updated=%s\n", sess.ID, sess.State, exit, sess.ProviderID, sess.UpdatedAt)
	res, err := c.SessionLog(ctx, id, client.SessionLogOptions{Limit: api.MaxSessionLogBytes})
	if err != nil {
		return err
	}
	for _, l := range sessionpeek.Digest(res.Data, n) {
		fmt.Fprintln(w, l)
	}
	return nil
}

var sessionsPeekCmd = &cobra.Command{
	Use:   "peek <id> [n]",
	Short: "Digest of a session's recent stream-json output (state, init, tool calls, results, final) with secrets redacted; works with --target",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		n := peekDefaultLines
		if len(args) == 2 {
			v, err := strconv.Atoi(args[1])
			if err != nil || v < 1 {
				return fmt.Errorf("n must be a positive integer, got %q", args[1])
			}
			n = v
		}
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return err
		}
		cmd.SilenceUsage = true
		return runSessionsPeek(cmd.Context(), cmd.OutOrStdout(), c, args[0], n)
	},
}
