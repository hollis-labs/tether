package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/spf13/cobra"
)

// newServiceTokenCommand manages service tokens (CW-20261019-0044): long-lived
// kind=service JWTs for an app, issued by the operator over the local socket
// and revoked through the deny-list. A token is shown once, at issue; list
// shows metadata only.
func newServiceTokenCommand(factory deviceAdminFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "service-token", Short: "Issue and list service tokens for apps (operator, local socket)"}
	var req api.ServiceTokenRequest
	var outFile string
	var asJSON bool
	issue := &cobra.Command{Use: "issue", Short: "Issue a service token; the token is printed once", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := factory()
			if err != nil {
				return err
			}
			var out api.ServiceTokenResponse
			if err := client.call(cmd.Context(), http.MethodPost, api.ServiceTokensPath, req, &out); err != nil {
				return err
			}
			if outFile != "" {
				// The credential goes to a new 0600 file, never to a terminal or a log.
				f, err := os.OpenFile(outFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator chose this path for their own credential file
				if err != nil {
					return fmt.Errorf("token file must not exist and must be creatable: %w", err)
				}
				if _, err := fmt.Fprintln(f, out.Token.Token); err != nil {
					_ = f.Close()
					return err
				}
				if err := f.Close(); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "token written to %s (jti %s, sub %s)\n", outFile, out.Record.JTI, out.Record.Sub)
				return nil
			}
			if asJSON {
				return writeJSONLine(cmd.OutOrStdout(), out)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "issued jti %s for %s (aud %s, scopes %v); shown once\n", out.Record.JTI, out.Record.Sub, out.Record.Audience, out.Record.Scopes)
			_, err = fmt.Fprintln(cmd.OutOrStdout(), out.Token.Token)
			return err
		}}
	issue.Flags().StringVar(&req.App, "app", "", "the app the token is for; the principal is msg://service/<app>/<env>")
	issue.Flags().StringVar(&req.Env, "env", "", "environment segment of the principal (default dev)")
	issue.Flags().StringVar(&req.Audience, "audience", "tether", "the app that verifies the token (tether for Tether's own API)")
	issue.Flags().StringSliceVar(&req.Scopes, "scope", nil, "scope, <audience>.<verb> (for tether: events, read, messages); repeatable")
	issue.Flags().StringVar(&req.Label, "label", "", "short label shown in listings")
	issue.Flags().BoolVar(&req.AllowWildcard, "allow-wildcard", false, "dev only: allow <audience>.*")
	issue.Flags().StringVar(&outFile, "out-file", "", "write the token to this new 0600 file instead of stdout")
	issue.Flags().BoolVar(&asJSON, "json", false, "print the token and its metadata as JSON")
	cmd.AddCommand(issue)
	var revokeApp, revokeEnv string
	revoke := &cobra.Command{Use: "revoke [jti]", Short: "Revoke one service token by jti, or all of an app's with --app", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := api.ServiceTokenRevokeRequest{App: revokeApp, Env: revokeEnv}
			if len(args) == 1 {
				req.JTI = args[0]
			}
			if (req.JTI == "") == (req.App == "") {
				return fmt.Errorf("give exactly one of a jti argument or --app")
			}
			client, err := factory()
			if err != nil {
				return err
			}
			var out api.ServiceTokenRevokeResponse
			if err := client.call(cmd.Context(), http.MethodPost, api.ServiceTokensPath+"/revoke", req, &out); err != nil {
				return err
			}
			for _, r := range out.Revoked {
				fmt.Fprintf(cmd.OutOrStdout(), "revoked %s (%s)\n", r.JTI, r.Sub)
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "verifiers stop accepting it within their deny-list poll interval (30s by default)")
			return nil
		}}
	revoke.Flags().StringVar(&revokeApp, "app", "", "revoke every live token of this app instead of one jti")
	revoke.Flags().StringVar(&revokeEnv, "env", "", "environment of --app (default dev)")
	cmd.AddCommand(revoke)
	var listJSON bool
	cmd.AddCommand(&cobra.Command{Use: "list", Short: "List service token metadata (never the tokens)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := factory()
			if err != nil {
				return err
			}
			var out api.ServiceTokenListResponse
			if err := client.call(cmd.Context(), http.MethodGet, api.ServiceTokensPath, nil, &out); err != nil {
				return err
			}
			if listJSON {
				return writeJSONLine(cmd.OutOrStdout(), out)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "JTI\tPRINCIPAL\tAUDIENCE\tSCOPES\tISSUED\tEXPIRES\tLABEL")
			for _, r := range out.Tokens {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\t%s\t%s\n", r.JTI, r.Sub, r.Audience, r.Scopes, r.IssuedAt.Format(time.RFC3339), r.ExpiresAt.Format("2006-01-02"), r.Label+revokedMark(r))
			}
			return tw.Flush()
		}})
	cmd.Commands()[len(cmd.Commands())-1].Flags().BoolVar(&listJSON, "json", false, "print as JSON")
	return cmd
}

func writeJSONLine(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }

func revokedMark(r authissuer.ServiceRecord) string {
	if r.RevokedAt == nil {
		return ""
	}
	return " [revoked " + r.RevokedAt.Format(time.RFC3339) + "]"
}
