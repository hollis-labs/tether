// Package mcpadapter — catalog_apply_tools.go wires the approval-gated catalog
// change tools (CW-20261012-0124): plan a bundle, read a plan, apply an
// approved plan, list backups. The daemon owns every write; these tools only
// call it. There is deliberately no approve tool: a plan is approved by the
// operator at a terminal (`tether catalog approve`), with a passphrase no
// agent session holds.
package mcpadapter

import (
	"context"
	"fmt"

	gomcp "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/catalogapply"
)

func (a *Adapter) registerCatalogApplyTools(s *gomcp.Server) {
	fileItem := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path":    map[string]interface{}{"type": "string", "description": "Path relative to the catalog root, e.g. agents/helper.yaml or launches/helper.yaml."},
			"content": map[string]interface{}{"type": "string", "description": "Full file content. Omit when delete is true."},
			"delete":  map[string]interface{}{"type": "boolean", "description": "Remove the file."},
		},
		"required": []string{"path"},
	}
	a.addTool(s, gomcp.Tool{
		Name: "tether_catalog_plan",
		Description: "Review a catalog change without applying it. Send a bundle of whole files (create, replace or delete) under the catalog root, or rollback_of to restore a backup. " +
			"The daemon checks every path against an allowlist (agents, launches, projects, providers, skills, boot-profiles, mcp-servers, sandbox-profiles, team-definitions, global.yaml, teams-policy.yaml; never credentials/), refuses obvious secrets, applies the bundle to a scratch copy, validates it and returns a diff, the digest, which changes are privileged (permissions, network, scopes, daemon defaults) and which running sessions use the touched definitions. Nothing live changes. " +
			"You cannot change your own agent or launch definition. A plan is installed only after the operator approves it by digest at a terminal; ask them to run `tether catalog approve <plan_id>`. Requires the catalog.write scope.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("name", "Short bundle name: letters, digits, '.', '_' or '-'. Used in the backup name. Required unless rollback_of is set.", false),
			gomcp.StringProp("task_id", "Torque task id the change belongs to (for the audit trail).", false),
			gomcp.ArrayProp("files", "Files to create, replace or delete.", false, fileItem),
			gomcp.StringProp("rollback_of", "Backup id (from tether_catalog_backups) to restore instead of sending files.", false),
		),
		Handler: a.handleCatalogPlan,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "tether_catalog_plan_get",
		Description: "Read a catalog plan: its state (planned, invalid, approved, applied), digest, diff and validation. Read-only; no scope required.",
		InputSchema: gomcp.InputSchema(gomcp.StringProp("plan_id", "Plan id from tether_catalog_plan.", true)),
		Handler:     a.handleCatalogPlanGet,
	}, Reads("catalog plan lookup"))

	a.addTool(s, gomcp.Tool{
		Name: "tether_catalog_apply",
		Description: "Install a catalog plan the operator has approved. Refused unless the plan is approved, unexpired and the live catalog still matches what was reviewed. " +
			"Backs up the whole catalog first, installs file by file, restores the touched files if anything fails or the catalog no longer validates, and returns the backup id, the files changed and an audit note to post on the Torque task. " +
			"activation=restart_required means the daemon still runs the old definitions until it is restarted; that restart is a separate step the operator approves. Requires the catalog.write scope.",
		InputSchema: gomcp.InputSchema(gomcp.StringProp("plan_id", "Approved plan id.", true)),
		Handler:     a.handleCatalogApply,
	}, Destroys("replaces catalog files; the previous catalog is kept as a backup and can be restored with tether_catalog_plan rollback_of"))

	a.addTool(s, gomcp.Tool{
		Name:        "tether_catalog_backups",
		Description: "List catalog backups, newest first. Each is the whole catalog as it was before an apply; pass its id as rollback_of to tether_catalog_plan. Read-only; no scope required.",
		InputSchema: gomcp.EmptyObjectSchema(),
		Handler:     a.handleCatalogBackups,
	}, Reads("catalog backup listing"))
}

func (a *Adapter) catalogApplyClient(tool string) error {
	if a.client == nil {
		return fmt.Errorf("%s requires daemon routing; start MCP with tether mcp", tool)
	}
	return nil
}

func (a *Adapter) handleCatalogPlan(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeCatalogWrite); err != nil {
		return nil, err
	}
	if err := a.catalogApplyClient("tether_catalog_plan"); err != nil {
		return nil, toolError("internal_error", err.Error())
	}
	req := api.CatalogPlanRequest{Name: str(args, "name"), TaskID: str(args, "task_id"), RollbackOf: str(args, "rollback_of")}
	if raw, ok := args["files"].([]any); ok {
		for i, item := range raw {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, toolError("invalid_argument", fmt.Sprintf("files[%d] must be an object with path and content", i))
			}
			del, _ := m["delete"].(bool)
			req.Files = append(req.Files, catalogapply.File{Path: str(m, "path"), Content: str(m, "content"), Delete: del})
		}
	}
	if req.RollbackOf == "" && (req.Name == "" || len(req.Files) == 0) {
		return nil, toolError("invalid_argument", "name and files are required (or rollback_of)")
	}
	plan, err := a.client.CatalogApply().Plan(ctx, req)
	if err != nil {
		return nil, a.catalogApplyErr(err)
	}
	return toolJSON(plan), nil
}

func (a *Adapter) handleCatalogPlanGet(ctx context.Context, args map[string]any) (any, error) {
	id := str(args, "plan_id")
	if id == "" {
		return nil, toolError("invalid_argument", "plan_id is required")
	}
	if err := a.catalogApplyClient("tether_catalog_plan_get"); err != nil {
		return nil, toolError("internal_error", err.Error())
	}
	plan, err := a.client.CatalogApply().Get(ctx, id)
	if err != nil {
		return nil, a.catalogApplyErr(err)
	}
	return toolJSON(plan), nil
}

func (a *Adapter) handleCatalogApply(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeCatalogWrite); err != nil {
		return nil, err
	}
	id := str(args, "plan_id")
	if id == "" {
		return nil, toolError("invalid_argument", "plan_id is required")
	}
	if err := a.catalogApplyClient("tether_catalog_apply"); err != nil {
		return nil, toolError("internal_error", err.Error())
	}
	res, err := a.client.CatalogApply().Apply(ctx, id)
	if err != nil {
		return nil, a.catalogApplyErr(err)
	}
	return toolJSON(res), nil
}

func (a *Adapter) handleCatalogBackups(ctx context.Context, _ map[string]any) (any, error) {
	if err := a.catalogApplyClient("tether_catalog_backups"); err != nil {
		return nil, toolError("internal_error", err.Error())
	}
	backups, err := a.client.CatalogApply().Backups(ctx)
	if err != nil {
		return nil, a.catalogApplyErr(err)
	}
	return toolJSON(map[string]any{"backups": backups}), nil
}

func (a *Adapter) catalogApplyErr(err error) error {
	if isDaemonUnreachable(err) {
		return daemonUnreachableError(err)
	}
	return toolError("catalog_change_refused", err.Error())
}
