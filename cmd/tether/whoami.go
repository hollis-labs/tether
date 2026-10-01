package main

// whoami.go — T08 (messaging vNext, CW-20260906-0039): CLI parity for
// GET /whoami (internal/api/whoami.go). Same-host, self-asserted trust
// model (ADR 0045) -- --as is whatever identity the caller claims.

import (
	"fmt"

	"github.com/spf13/cobra"
)

var whoamiAs string
var whoamiJSON bool

var whoamiCmd = &cobra.Command{
	Use:   "whoami",
	Short: "Self-discovery: your registry profile, identity mappings, memberships, and current binding",
	Long: `Show what the daemon knows about the identity you claim via --as:
the registered Profile (if any), attached external-id mappings, group
memberships, and the current RuntimeBinding (host/session currently
owning delivery), if any.

An unregistered or never-bound identity is not an error -- self-discovery
works for any msg:// URN, whether or not it has ever registered or leased
a binding.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if whoamiAs == "" {
			return validationErr("whoami: --as <urn> is required")
		}
		// Shares registry.go's test seam (registryClientFactory) --
		// its own doc comment calls this out explicitly: "lets us share
		// the same factory whether the caller wants Registry() or any
		// other typed accessor we add later."
		c, err := registryClientFactory()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.Whoami(cmdCtx(cmd), whoamiAs)
		if err != nil {
			return classifyErr(err)
		}
		if whoamiJSON {
			return printJSON(out)
		}
		fmt.Printf("urn: %s\n", out.URN)
		if out.Profile != nil {
			fmt.Printf("profile: %s (kind=%s status=%s)\n", out.Profile.DisplayName, out.Profile.Kind, out.Profile.Status)
		} else {
			fmt.Println("profile: (not registered)")
		}
		if len(out.ExternalIDs) == 0 {
			fmt.Println("external_ids: (none)")
		}
		for _, id := range out.ExternalIDs {
			fmt.Printf("external_id: %s/%s\n", id.Substrate, id.ExternalID)
		}
		if len(out.Groups) == 0 {
			fmt.Println("groups: (none)")
		}
		for _, g := range out.Groups {
			fmt.Printf("group: %s (%s)\n", g.DisplayName, g.URN)
		}
		if out.Binding != nil {
			fmt.Printf("binding: session_id=%s host_id=%s generation=%d visibility=%s\n",
				out.Binding.SessionID, out.Binding.HostID, out.Binding.Generation, out.Binding.Visibility)
		} else {
			fmt.Println("binding: (none)")
		}
		return nil
	},
}

func init() {
	whoamiCmd.Flags().StringVar(&whoamiAs, "as", "", "msg:// URN to look up (self-asserted, no verification)")
	whoamiCmd.Flags().BoolVar(&whoamiJSON, "json", false, "emit raw JSON instead of the pretty rendering")
	rootCmd.AddCommand(whoamiCmd)
}
