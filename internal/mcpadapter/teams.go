package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	"github.com/hollis-labs/libs/mesh/core/teams"
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
		description := "Team " + verb + " through the authenticated daemon service. Requires a caller-scoped idempotency key; retries retain the original result. No asserted caller identity is accepted."
		if verb == "form" {
			description += " request.team is a versioned definition with id, name, version, slots, phases, authority, routing and policy; coordinator/member roster shortcuts are not supported. Stage A uses coordinator/workers/reviewer slots and operator-approved definition pins."
		}
		behavior := Writes()
		if verb == "dissolve" || verb == "member_remove" || verb == "cancel" {
			behavior = Destroys("Ends team members and releases their resources")
		}
		a.addTool(s, gomcp.Tool{
			Name:        "tether_team_" + verb,
			Description: description,
			InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"key", "request"}, "properties": map[string]any{
				"key":          map[string]any{"type": "string", "minLength": 1, "maxLength": teamsvc.MaxKeyBytes, "description": "Caller-scoped idempotency key"},
				"request":      teamRequestSchema(verb),
				"_traceparent": map[string]any{"type": "string"}, "_tracestate": map[string]any{"type": "string"},
			}},
			OutputSchema: map[string]any{"type": "object", "description": "The stable team service Result: run metadata, minimal membership views and delivery keys; no host intents or session identifiers.", "properties": map[string]any{"run": map[string]any{"type": "object"}, "member": map[string]any{"type": "object"}, "recipients": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}, "delivery_keys": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "self": map[string]any{"type": "string"}, "coordinator": map[string]any{"type": "string"}, "channel": map[string]any{"type": "string"}, "members": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}}, "additionalProperties": false},
			Handler:      func(ctx context.Context, args map[string]any) (any, error) { return a.handleTeam(ctx, verb, args) },
		}, behavior)
	}
}

func teamRequestSchema(verb string) map[string]any {
	fields, required := api.TeamRequestFields(verb), api.TeamRequiredFields(verb)
	properties := map[string]any{}
	for _, field := range fields {
		if field == "team" {
			properties[field] = teamInputSchema[teams.Team]()
			continue
		}
		if field == "launch" {
			properties[field] = teamInputSchema[api.TeamLaunchRequest]()
			continue
		}
		kind := "string"
		if field == "limits" {
			kind = "object"
		}
		if field == "cascade" {
			kind = "boolean"
		}
		prop := map[string]any{"type": kind}
		switch field {
		case "kind":
			prop["enum"] = []string{"agent", "user", "service"}
			prop["description"] = "Optional recipient actor-kind filter; omit for any"
		case "history":
			prop["enum"] = []string{"none", "summary", "filtered", "full"}
			prop["description"] = "Optional; may not widen the team's history policy. Omit unless needed"
		case "address":
			prop["description"] = "@slot, @slot*, @all or @member-id; the grants must allow your slot to reach it"
		}
		properties[field] = prop
	}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
}

// Describe nested fields from their wire types. Leave required/zero-value
// semantics to the shared decoder and service, as for HTTP and CLI callers.
func teamInputSchema[T any]() *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err) // These fixed request types must remain representable as JSON.
	}
	var describe func(*jsonschema.Schema)
	describe = func(s *jsonschema.Schema) {
		if s == nil {
			return
		}
		s.Required = nil
		if s.Type == "array" || s.Type == "object" {
			s.Types, s.Type = []string{s.Type, "null"}, ""
		}
		for _, child := range s.Properties {
			describe(child)
		}
		for _, child := range s.Defs {
			describe(child)
		}
		describe(s.Items)
		describe(s.AdditionalProperties)
	}
	describe(schema)
	return schema
}

func (a *Adapter) handleTeam(ctx context.Context, verb string, args map[string]any) (any, error) {
	// Daemon-backed ops authenticate with the session's own credential and the
	// daemon enforces its grants; the adapter's local scope list is not authority.
	if _, remote := a.teams.(clientTeamOps); !remote {
		if err := a.checkScope(ScopeTeamWrite); err != nil {
			return nil, err
		}
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
		status, detail := api.TeamErrorFor(ctx, err)
		if status >= 500 {
			slog.WarnContext(ctx, "team verb failed", "verb", verb, "code", detail.Code, "cause", api.InternalCause(err))
		}
		return nil, toolError(detail.Code, detail.Message)
	}
	return result, nil
}
