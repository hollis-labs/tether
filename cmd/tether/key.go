package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/spf13/cobra"
)

func newKeyCommand(factory deviceAdminFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "key", Short: "Inspect registered credential metadata"}
	var q identity.CredentialQuery
	list := &cobra.Command{Use: "list", Short: "List credential metadata, including expired and revoked credentials", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if q.Limit < 1 {
				return fmt.Errorf("limit must be between 1 and 200")
			}
			if err := q.Validate(); err != nil {
				return err
			}
			client, err := factory()
			if err != nil {
				return err
			}
			values := url.Values{"limit": {strconv.Itoa(q.Limit)}, "after": {strconv.FormatInt(q.After, 10)}}
			if q.Kind != "" {
				values.Set("kind", q.Kind)
			}
			if q.PrincipalID != "" {
				values.Set("principal", q.PrincipalID)
			}
			var page identity.CredentialPage
			if err := client.call(cmd.Context(), http.MethodGet, "/auth/keys?"+values.Encode(), nil, &page); err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(page)
		},
	}
	list.Flags().StringVar(&q.Kind, "kind", "", "filter by principal kind: operator, session, service, interactive, device")
	list.Flags().StringVar(&q.PrincipalID, "principal", "", "filter by exact principal subject")
	list.Flags().Int64Var(&q.After, "after", 0, "continue from the previous page's next_after credential ID")
	list.Flags().IntVar(&q.Limit, "limit", 100, "maximum credentials per page (1-200)")
	cmd.AddCommand(list)
	return cmd
}
