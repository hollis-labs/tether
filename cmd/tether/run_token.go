package main

import (
	"fmt"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/spf13/cobra"
	"net/http"
)

func newRunTokenCommand(factory deviceAdminFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "runtoken", Short: "Manage execution run proofs (operator, local socket)"}
	cmd.AddCommand(&cobra.Command{Use: "revoke <jti|run>", Short: "Permanently revoke an exact jti or execution run id", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !store.ValidRunTokenTarget(args[0]) {
			return store.ErrRunTokenTarget
		}
		client, err := factory()
		if err != nil {
			return err
		}
		req := api.RunTokenRevokeRequest{Target: args[0]}
		if err := client.call(cmd.Context(), http.MethodPost, api.RunTokenRevokePath, req, nil); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "revoked", args[0])
		return err
	}})
	return cmd
}
