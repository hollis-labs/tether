package main

// session_refs.go — CLI parity for /sessions/{id}/refs and the
// /workstreams/{id}/refs roll-up.
//
// S2 of SP-20260912-0001 (CW-20260912-0060).

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/api"
)

var (
	refURI          string
	refRelation     string
	refKindFilter   string
	refSourceFilter string
	refWorkstream   string
	refJSON         bool
)

var refsCmd = &cobra.Command{
	Use:   "refs",
	Short: "What a session touched (Torque tasks, commits, revisions, deploys)",
	Long: `Refs record what a session created, updated, read or referenced.

They attach to a session and roll up through its workstream, which is what makes
them survive a compaction -- a compaction creates a NEW session row, so a
roll-up keyed on the session id alone would lose everything before it.`,
}

var refAttachCmd = &cobra.Command{
	Use:   "attach <session-id> <kind> <ref-id>",
	Short: "Record that a session touched an object",
	Long: `Idempotent: re-attaching the same (kind, ref-id, relation) is a no-op, so a
hook that runs twice is safe. A repeat does not revise the original -- the row
records what was asserted at the time.

This is the surface the commit / PR / end-session hooks call. Git is out of the
proxy's reach entirely (commits happen via Bash, which never reaches Tether), so
commit and PR refs arrive this way or not at all.`,
	Args: cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := registryClientFactory()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AttachSessionRef(cmdCtx(cmd), args[0], api.SessionRefAttachRequest{
			Kind:     args[1],
			RefID:    args[2],
			URI:      refURI,
			Relation: refRelation,
		})
		if err != nil {
			return classifyErr(err)
		}
		if refJSON {
			return printJSON(out)
		}
		switch {
		case out.Inserted:
			fmt.Printf("attached %s %s (%s) to session %s\n", out.Ref.Kind, out.Ref.RefID, out.Ref.Relation, args[0])
		case out.Upgraded:
			fmt.Printf("upgraded %s %s (%s) to proxy-observed\n", out.Ref.Kind, out.Ref.RefID, out.Ref.Relation)
		default:
			fmt.Printf("already attached: %s %s (%s)\n", out.Ref.Kind, out.Ref.RefID, out.Ref.Relation)
		}
		return nil
	},
}

var refListCmd = &cobra.Command{
	Use:   "list [session-id]",
	Short: "List a session's refs, or a workstream roll-up with --workstream",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 && refWorkstream == "" {
			return validationErr("refs list: give a session id or --workstream <id>")
		}
		if len(args) == 1 && refWorkstream != "" {
			return validationErr("refs list: give a session id or --workstream, not both")
		}
		c, err := registryClientFactory()
		if err != nil {
			return classifyErr(err)
		}
		var refs []api.SessionRefDTO
		if refWorkstream != "" {
			refs, err = c.ListWorkstreamRefs(cmdCtx(cmd), refWorkstream, refKindFilter, refRelation, refSourceFilter)
		} else {
			refs, err = c.ListSessionRefs(cmdCtx(cmd), args[0], refKindFilter, refRelation, refSourceFilter)
		}
		if err != nil {
			return classifyErr(err)
		}
		if refJSON {
			return printJSON(refs)
		}
		if len(refs) == 0 {
			// Absence is not a finding. Direct MCP children, HTTP callers and
			// the CLI all bypass the proxy, so real work can leave no refs.
			fmt.Println("(no refs)")
			return nil
		}
		fmt.Printf("%-22s %-24s %-11s %-7s %s\n", "KIND", "REF", "RELATION", "SOURCE", "AT")
		for _, r := range refs {
			fmt.Printf("%-22s %-24s %-11s %-7s %s\n", r.Kind, r.RefID, r.Relation, r.Source, r.At)
		}
		return nil
	},
}

func init() {
	refAttachCmd.Flags().StringVar(&refURI, "uri", "", "optional resolvable locator")
	refAttachCmd.Flags().StringVar(&refRelation, "relation", "", "created | updated | read | referenced (default referenced)")
	refAttachCmd.Flags().BoolVar(&refJSON, "json", false, "print JSON")

	refListCmd.Flags().StringVar(&refWorkstream, "workstream", "", "list the workstream roll-up instead of one session")
	refListCmd.Flags().StringVar(&refKindFilter, "kind", "", "filter by kind")
	refListCmd.Flags().StringVar(&refRelation, "relation", "", "filter by relation")
	refListCmd.Flags().StringVar(&refSourceFilter, "source", "", "filter by source: proxy, api or agent")
	refListCmd.Flags().BoolVar(&refJSON, "json", false, "print JSON")

	refsCmd.AddCommand(refAttachCmd, refListCmd)
	sessionsCmd.AddCommand(refsCmd)
}
