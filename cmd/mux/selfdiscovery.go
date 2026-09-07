package main

// selfdiscovery.go — T08 (messaging vNext, CW-20260906-0039): CLI
// exposure for GET /whoami (self-discovery) and POST /bootstrap/session
// (the provider-neutral local bootstrap/registration helper). Both are
// entirely new capabilities, never built before this task.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/client"
)

var whoAmICmd = &cobra.Command{
	Use:   "whoami <urn>",
	Short: "Self-discovery: report what Tether currently knows about a claimed URN",
	Long: `Same-host, self-asserted trust model (ADR 0045): the URN is a claim, not
a verified identity. Reports the registry profile, current RuntimeBinding,
group memberships, and (when it currently resolves to a session THIS
daemon's runtime is actually running) the session id, host, and
negotiated provider capabilities -- whatever is independently true, with
no requirement that any of it exist.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.WhoAmI(cmdCtx(cmd), args[0])
		if err != nil {
			return classifyErr(err)
		}
		return printJSON(out)
	},
}

var (
	bootstrapSessSessionID      string
	bootstrapSessIntent         string
	bootstrapSessLogicalAgentID string
	bootstrapSessDisplayName    string
	bootstrapSessProviderID     string
	bootstrapSessPublication    string
	bootstrapSessHostID         string
	bootstrapSessAttemptID      string
	bootstrapSessSubstrate      string
)

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Provider-neutral local bootstrap/registration helper for the launch/host boundary",
	Long: `Idempotently resolves a preassigned SESSION into (optionally) a registered,
bound durable actor identity, in one call -- for a launch/host integration
(e.g. agent-setup) to invoke instead of hand-rolling registration + binding
sequencing itself.

--logical-agent-id absent: standalone, session-only boot. No registry
write, no binding -- "a one-off session need not manufacture a permanent
actor record merely to send a message."

--logical-agent-id present, --publication omitted or "private-local":
private offline boot. Identity by naming only; still no registry write or
binding -- Tether's own internal launch path is the only way to mint a
private-local binding.

--logical-agent-id present, --publication published-local: registers
(idempotently, by --logical-agent-id) and leases a binding (capabilities
locked to pull-only; push-notified bridging is not implemented). A
repeated call with the same --session-id/--host-id/--attempt-id is a
true no-op, not a wasted new generation -- a genuinely new --attempt-id
is treated as a legitimate reconnect.

Calling code on an unreachable daemon should fall back to a purely local
SESSION identity and proceed with the launch anyway -- bootstrap and
local messaging must never require online Tether.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if bootstrapSessSessionID == "" {
			return validationErr("bootstrap: --session-id is required")
		}
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.BootstrapSession(cmdCtx(cmd), client.BootstrapSessionRequest{
			SessionID:      bootstrapSessSessionID,
			Intent:         bootstrapSessIntent,
			LogicalAgentID: bootstrapSessLogicalAgentID,
			DisplayName:    bootstrapSessDisplayName,
			ProviderID:     bootstrapSessProviderID,
			Publication:    bootstrapSessPublication,
			HostID:         bootstrapSessHostID,
			AttemptID:      bootstrapSessAttemptID,
			Substrate:      bootstrapSessSubstrate,
		})
		if err != nil {
			return classifyErr(err)
		}
		if out.ReusedBinding {
			fmt.Fprintln(cmd.OutOrStdout(), "# idempotent repeat: reused the existing current binding")
		}
		return printJSON(out)
	},
}

func init() {
	bootstrapCmd.Flags().StringVar(&bootstrapSessSessionID, "session-id", "", "the preassigned SESSION identity (required)")
	bootstrapCmd.Flags().StringVar(&bootstrapSessIntent, "intent", "", "informational passthrough: preassigned, resume, fresh, fork, compact")
	bootstrapCmd.Flags().StringVar(&bootstrapSessLogicalAgentID, "logical-agent-id", "", "presence implies a durable actor, not a one-off session")
	bootstrapCmd.Flags().StringVar(&bootstrapSessDisplayName, "display-name", "", "registry display name; defaults to --logical-agent-id")
	bootstrapCmd.Flags().StringVar(&bootstrapSessProviderID, "provider-id", "", "optional; accepted but not consumed by this call")
	bootstrapCmd.Flags().StringVar(&bootstrapSessPublication, "publication", "", "private-local (default) or published-local")
	bootstrapCmd.Flags().StringVar(&bootstrapSessHostID, "host-id", "", "defaults to \"external-bootstrap\" when a binding is leased")
	bootstrapCmd.Flags().StringVar(&bootstrapSessAttemptID, "attempt-id", "", "defaults to --session-id; a new value signals a reconnect")
	bootstrapCmd.Flags().StringVar(&bootstrapSessSubstrate, "substrate", "", "defaults to \"agent-setup\"")

	rootCmd.AddCommand(whoAmICmd)
	rootCmd.AddCommand(bootstrapCmd)
}
