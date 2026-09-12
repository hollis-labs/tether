package main

// workstreams.go — CLI parity for /workstreams and
// /sessions/{id}/workstream (internal/api/workstreams.go), matching the
// tether_workstream_* MCP tools.
//
// S1 of SP-20260912-0001 (CW-20260912-0059).

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/api"
)

var (
	workstreamName       string
	workstreamWorkflowID string
	workstreamStatus     string
	workstreamUser       string
	workstreamMemoryType string
	workstreamJSON       bool
)

var workstreamsCmd = &cobra.Command{
	Use:   "workstreams",
	Short: "Workstream commands (the durable container a unit of work lives in)",
	Long: `A workstream is the container for work that outlives any one session.

A compaction creates a NEW session row, so anything attached to a session id is
orphaned by the exact event it was meant to survive. A resume/compact/fork child
inherits its parent's workstream structurally, so a container attached here spans
the whole lineage.`,
}

var workstreamCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a workstream",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := registryClientFactory()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.CreateWorkstream(cmdCtx(cmd), workstreamName, workstreamWorkflowID)
		if err != nil {
			return classifyErr(err)
		}
		if workstreamJSON {
			return printJSON(out)
		}
		printWorkstream(out)
		return nil
	},
}

var workstreamGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Show one workstream",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := registryClientFactory()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.GetWorkstream(cmdCtx(cmd), args[0])
		if err != nil {
			return classifyErr(err)
		}
		if workstreamJSON {
			return printJSON(out)
		}
		printWorkstream(out)
		return nil
	},
}

var workstreamListCmd = &cobra.Command{
	Use:   "list",
	Short: "List workstreams, newest first",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := registryClientFactory()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.ListWorkstreams(cmdCtx(cmd), workstreamStatus, workstreamWorkflowID)
		if err != nil {
			return classifyErr(err)
		}
		if workstreamJSON {
			return printJSON(out)
		}
		if len(out) == 0 {
			fmt.Println("(no workstreams)")
			return nil
		}
		fmt.Printf("%-38s %-10s %-20s %s\n", "ID", "STATUS", "WORKFLOW", "NAME")
		for _, w := range out {
			fmt.Printf("%-38s %-10s %-20s %s\n", w.ID, w.Status, dashIfEmpty(w.WorkflowID), dashIfEmpty(w.Name))
		}
		return nil
	},
}

var workstreamAssignCmd = &cobra.Command{
	Use:   "assign <session-id> <workstream-id>",
	Short: "Assign a session to a workstream",
	Long: `Stamp a workstream onto a session.

Pass an empty workstream id ("") to clear the association.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := registryClientFactory()
		if err != nil {
			return classifyErr(err)
		}
		if err := c.AssignSessionWorkstream(cmdCtx(cmd), args[0], args[1]); err != nil {
			return classifyErr(err)
		}
		if args[1] == "" {
			fmt.Printf("cleared workstream for session %s\n", args[0])
		} else {
			fmt.Printf("assigned session %s to workstream %s\n", args[0], args[1])
		}
		return nil
	},
}

var workstreamEnsureCmd = &cobra.Command{
	Use:   "ensure <session-id>",
	Short: "Get the session's workstream, creating one for the lineage if it has none",
	Long: `The one-call path for a session that needs a container without ceremony.

Idempotent: a second call returns the same workstream rather than minting a new
one. When the lineage has no workstream, one is created and stamped onto every
session from the lineage root down, so the session's own parent is inside the
container too.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := registryClientFactory()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.EnsureSessionWorkstream(cmdCtx(cmd), args[0], workstreamName, workstreamWorkflowID)
		if err != nil {
			return classifyErr(err)
		}
		if workstreamJSON {
			return printJSON(out)
		}
		printWorkstream(out)
		return nil
	},
}

var workstreamNamespaceCmd = &cobra.Command{
	Use:   "namespace <session-id>",
	Short: "Where this session's workstream-scoped scratch belongs in Tesseract",
	Long: `Resolve the Tesseract namespace for a session's workstream-scoped content.

Tether returns the location and stores none of the content — write it to
Tesseract yourself, then attach the returned revision id with
` + "`mux sessions refs attach`" + `.

The namespace keys on the WORKSTREAM, not the session, which is what makes a
note written before a compaction readable after one. The id in the session
segment carries a ws_ prefix and is a workstream id: it will never match a
session id, deliberately, because a bare id there would fail silently instead.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if workstreamUser == "" {
			return validationErr("workstreams namespace: --user is required; Tether does not own the Tesseract user identity")
		}
		c, err := registryClientFactory()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.SessionWorkstreamNamespace(cmdCtx(cmd), args[0], workstreamUser, workstreamMemoryType)
		if err != nil {
			return classifyErr(err)
		}
		if workstreamJSON {
			return printJSON(out)
		}
		fmt.Println(out.Namespace)
		return nil
	},
}

func printWorkstream(w api.WorkstreamDTO) {
	fmt.Printf("id:         %s\n", w.ID)
	fmt.Printf("name:       %s\n", dashIfEmpty(w.Name))
	fmt.Printf("workflow:   %s\n", dashIfEmpty(w.WorkflowID))
	fmt.Printf("status:     %s\n", w.Status)
	fmt.Printf("created_at: %s\n", w.CreatedAt)
}

func init() {
	workstreamCreateCmd.Flags().StringVar(&workstreamName, "name", "", "optional human label")
	workstreamCreateCmd.Flags().StringVar(&workstreamWorkflowID, "workflow-id", "", "optional workflow correlation id (free-form; never resolved by Tether)")
	workstreamCreateCmd.Flags().BoolVar(&workstreamJSON, "json", false, "print JSON")

	workstreamGetCmd.Flags().BoolVar(&workstreamJSON, "json", false, "print JSON")

	workstreamListCmd.Flags().StringVar(&workstreamStatus, "status", "", "filter by status: active or closed")
	workstreamListCmd.Flags().StringVar(&workstreamWorkflowID, "workflow-id", "", "filter by workflow correlation id")
	workstreamListCmd.Flags().BoolVar(&workstreamJSON, "json", false, "print JSON")

	workstreamEnsureCmd.Flags().StringVar(&workstreamName, "name", "", "label used only if a workstream is created")
	workstreamEnsureCmd.Flags().StringVar(&workstreamWorkflowID, "workflow-id", "", "workflow id used only if a workstream is created")
	workstreamEnsureCmd.Flags().BoolVar(&workstreamJSON, "json", false, "print JSON")

	workstreamNamespaceCmd.Flags().StringVar(&workstreamUser, "user", "", "Tesseract user id (required)")
	workstreamNamespaceCmd.Flags().StringVar(&workstreamMemoryType, "type", "", "Tesseract memory type; defaults to notes")
	workstreamNamespaceCmd.Flags().BoolVar(&workstreamJSON, "json", false, "print JSON")

	workstreamsCmd.AddCommand(workstreamCreateCmd, workstreamGetCmd, workstreamListCmd, workstreamAssignCmd, workstreamEnsureCmd, workstreamNamespaceCmd)
}
