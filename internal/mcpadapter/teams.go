package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"

	gomcp "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

// SetTeams enables the team tool catalog with the host's single service.
// Hosts pass nil when teams are disabled. Configure before creating servers or registering tools.
func (a *Adapter) SetTeams(ops api.TeamOps) { a.teams = ops }

func (a *Adapter) registerTeamTools(s *gomcp.Server) {
	if !api.HasTeamOps(a.teams) {
		return
	}
	for _, verb := range api.TeamVerbs() {
		behavior := Writes()
		if verb == "dissolve" || verb == "member_remove" || verb == "cancel" {
			behavior = Destroys("Ends team members and releases their resources")
		}
		a.addTool(s, gomcp.Tool{
			Name:        "tether_team_" + verb,
			Description: "Team " + verb + " through the authenticated daemon service. Requires a caller-scoped idempotency key; retries retain the original result. No asserted caller identity is accepted.",
			InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"key", "request"}, "properties": map[string]any{
				"key":          map[string]any{"type": "string", "minLength": 1, "maxLength": teamsvc.MaxKeyBytes, "description": "Caller-scoped idempotency key"},
				"request":      teamRequestSchema(verb),
				"_traceparent": map[string]any{"type": "string"}, "_tracestate": map[string]any{"type": "string"},
			}},
			OutputSchema: map[string]any{"type": "object", "description": "The stable team service Result: run metadata, minimal membership views and delivery keys; no host intents or session identifiers.", "properties": map[string]any{"run": map[string]any{"type": "object"}, "member": map[string]any{"type": "object"}, "recipients": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}, "delivery_keys": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "additionalProperties": false},
			Handler:      func(ctx context.Context, args map[string]any) (any, error) { return a.handleTeam(ctx, verb, args) },
		}, behavior)
	}
}

func teamRequestSchema(verb string) map[string]any {
	fields, required := api.TeamRequestFields(verb), api.TeamRequiredFields(verb)
	properties := map[string]any{}
	for _, field := range fields {
		kind := "string"
		if field == "team" || field == "launch" || field == "limits" {
			kind = "object"
		}
		if field == "cascade" {
			kind = "boolean"
		}
		properties[field] = map[string]any{"type": kind}
	}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
}

func (a *Adapter) handleTeam(ctx context.Context, verb string, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeTeamWrite); err != nil {
		return nil, err
	}
	for _, field := range traceMetaKeys {
		if value, exists := args[field]; exists {
			if _, ok := value.(string); !ok {
				return nil, toolError("invalid_request", "invalid_request")
			}
		}
	}
	count := 0
	for field := range args {
		if field != "_traceparent" && field != "_tracestate" {
			count++
		}
	}
	key, ok := args["key"].(string)
	var err error
	var result teamsvc.Result
	if !ok || count != 2 {
		err = teamsvc.ErrInvalidRequest
	} else {
		var encoded bytes.Buffer
		enc := json.NewEncoder(&encoded)
		enc.SetEscapeHTML(false)
		if enc.Encode(args["request"]) != nil {
			err = teamsvc.ErrInvalidRequest
		} else {
			var request api.TeamRequest
			request, err = api.DecodeTeamRequest(verb, key, &encoded)
			if err == nil {
				result, err = api.CallTeam(ctx, a.teams, verb, key, request)
			}
		}
	}
	if err != nil {
		_, detail := api.TeamError(err)
		return nil, toolError(detail.Code, detail.Message)
	}
	return result, nil
}
