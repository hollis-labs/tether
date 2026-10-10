package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"

	gomcp "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"
	directory "github.com/hollis-labs/tether/internal/environmentdirectory"
)

func (a *Adapter) registerEnvironmentDirectoryTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{Name: "tether_environment_list", Description: "List daemon-owned environment records and observed state. Visibility and management mode grant no worker control; private credential references are redacted.", InputSchema: gomcp.InputSchema(), Handler: a.handleEnvironmentList}, Reads("daemon environment directory SELECT"))
	a.addTool(s, gomcp.Tool{Name: "tether_environment_get", Description: "Get an environment by its immutable UUID; private credential reference is redacted.", InputSchema: gomcp.InputSchema(gomcp.StringProp("environment_id", "Environment UUID.", true)), Handler: a.handleEnvironmentGet}, Reads("daemon environment directory SELECT"))
	a.addTool(s, gomcp.Tool{Name: "tether_environment_register", Description: "Register an explicitly authorized expected environment identity and durable routes. Requires a verified operator over the local Unix socket. Does not enroll actors, create grants or grant management authority.", InputSchema: gomcp.InputSchema(gomcp.ObjectProp("registration", "Expected UUID/authority, durable routes, private credential reference, exact device ID, deployment ownership and optional inert mode/home declarations.", true)), Handler: a.handleEnvironmentRegister}, Writes())
	a.addTool(s, gomcp.Tool{Name: "tether_environment_rename", Description: "Change only an environment's display label. Requires the local verified operator; immutable authority and UUID remain bound.", InputSchema: gomcp.InputSchema(gomcp.StringProp("environment_id", "Environment UUID.", true), gomcp.StringProp("label", "New display label.", true)), Handler: a.handleEnvironmentRename}, Writes())
	a.addTool(s, gomcp.Tool{Name: "tether_environment_remove", Description: "Retire an environment while retaining its identity/home tombstones. Requires the local verified operator. Revocation remains pending without a successful explicitly authorized worker revoke; retirement is not proof of computation death.", InputSchema: gomcp.InputSchema(gomcp.StringProp("environment_id", "Environment UUID.", true)), Handler: a.handleEnvironmentRemove}, Writes())
}

func (a *Adapter) handleEnvironmentList(ctx context.Context, _ map[string]any) (any, error) {
	if a.client == nil {
		return nil, toolError("unavailable", "environment directory requires daemon routing")
	}
	out, err := a.client.ListEnvironments(ctx)
	if err != nil {
		return nil, err
	}
	return toolJSON(map[string]any{"environments": out}), nil
}
func (a *Adapter) handleEnvironmentGet(ctx context.Context, args map[string]any) (any, error) {
	if a.client == nil {
		return nil, toolError("unavailable", "environment directory requires daemon routing")
	}
	id, _ := args["environment_id"].(string)
	out, err := a.client.GetEnvironment(ctx, id)
	if err != nil {
		return nil, err
	}
	return toolJSON(out), nil
}
func (a *Adapter) handleEnvironmentRegister(ctx context.Context, args map[string]any) (any, error) {
	if a.client == nil {
		return nil, toolError("unavailable", "environment directory requires daemon routing")
	}
	body, err := json.Marshal(args["registration"])
	if err != nil {
		return nil, toolError("invalid_request", "invalid registration")
	}
	var in directory.Registration
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&in); err != nil || string(body) == "null" {
		return nil, toolError("invalid_request", "invalid registration")
	}
	out, err := a.client.RegisterEnvironment(ctx, in)
	if err != nil {
		return nil, err
	}
	return toolJSON(out), nil
}
func (a *Adapter) handleEnvironmentRename(ctx context.Context, args map[string]any) (any, error) {
	if a.client == nil {
		return nil, toolError("unavailable", "environment directory requires daemon routing")
	}
	id, _ := args["environment_id"].(string)
	label, _ := args["label"].(string)
	out, err := a.client.RenameEnvironment(ctx, id, label)
	if err != nil {
		return nil, err
	}
	return toolJSON(out), nil
}
func (a *Adapter) handleEnvironmentRemove(ctx context.Context, args map[string]any) (any, error) {
	if a.client == nil {
		return nil, toolError("unavailable", "environment directory requires daemon routing")
	}
	id, _ := args["environment_id"].(string)
	out, err := a.client.RetireEnvironment(ctx, id)
	if err != nil {
		return nil, err
	}
	return toolJSON(out), nil
}
