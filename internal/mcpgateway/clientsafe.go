package mcpgateway

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ClientSafeTool returns t as a client must be able to parse it. Some upstreams
// serialize Go nil slices and maps as JSON null (`"required": null`), which
// JSON Schema forbids and strict MCP clients (Claude Code) reject for the whole
// tools/list. A null for a schema keyword means "absent", so dropping it
// changes no behavior. The registry keeps the upstream definition untouched;
// this runs where the list is handed to a client. t is returned as is when
// nothing needs to change; otherwise a copy is returned.
func ClientSafeTool(t *mcpsdk.Tool) *mcpsdk.Tool {
	if t == nil {
		return t
	}
	in, inChanged := cleanSchema(t.InputSchema)
	out, outChanged := cleanSchema(t.OutputSchema)
	if !inChanged && !outChanged {
		return t
	}
	cp := *t
	cp.InputSchema, cp.OutputSchema = in, out
	return &cp
}

// Keywords whose value is a schema, a map of schemas or a list of schemas.
var (
	schemaMapKeywords  = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true}
	schemaKeywords     = map[string]bool{"items": true, "additionalProperties": true, "not": true, "if": true, "then": true, "else": true, "contains": true, "propertyNames": true, "unevaluatedItems": true, "unevaluatedProperties": true, "additionalItems": true}
	schemaListKeywords = map[string]bool{"allOf": true, "anyOf": true, "oneOf": true, "prefixItems": true, "items": true}
	// A null here is a real value, not an absent keyword.
	nullValueKeywords = map[string]bool{"default": true, "const": true, "example": true, "examples": true, "enum": true}
)

func cleanSchema(schema any) (any, bool) {
	switch v := schema.(type) {
	case map[string]any:
		return cleanSchemaMap(v)
	case json.RawMessage:
		return cleanSchemaBytes(v)
	case []byte:
		return cleanSchemaBytes(v)
	}
	return schema, false
}

func cleanSchemaBytes(raw []byte) (any, bool) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return raw, false
	}
	cleaned, changed := cleanSchemaMap(m)
	if !changed {
		return raw, false
	}
	return cleaned, true
}

func cleanSchemaMap(m map[string]any) (map[string]any, bool) {
	var out map[string]any
	set := func(key string, value any, drop bool) {
		if out == nil {
			out = make(map[string]any, len(m))
			for k, v := range m {
				out[k] = v
			}
		}
		if drop {
			delete(out, key)
		} else {
			out[key] = value
		}
	}
	for key, value := range m {
		if value == nil {
			if !nullValueKeywords[key] {
				set(key, nil, true)
			}
			continue
		}
		switch {
		case schemaMapKeywords[key]:
			if sub, ok := value.(map[string]any); ok {
				var cleaned map[string]any
				for name, child := range sub {
					if c, changed := cleanSchemaValue(child); changed {
						if cleaned == nil {
							cleaned = make(map[string]any, len(sub))
							for k, v := range sub {
								cleaned[k] = v
							}
						}
						cleaned[name] = c
					}
				}
				if cleaned != nil {
					set(key, cleaned, false)
				}
			}
		case schemaKeywords[key] || schemaListKeywords[key]:
			if c, changed := cleanSchemaValue(value); changed {
				set(key, c, false)
			}
		}
	}
	if out == nil {
		return m, false
	}
	return out, true
}

func cleanSchemaValue(value any) (any, bool) {
	switch v := value.(type) {
	case map[string]any:
		return cleanSchemaMap(v)
	case []any:
		var out []any
		for i, item := range v {
			if c, changed := cleanSchemaValue(item); changed {
				if out == nil {
					out = append([]any(nil), v...)
				}
				out[i] = c
			}
		}
		if out == nil {
			return v, false
		}
		return out, true
	}
	return value, false
}

// A tool name every MCP client accepts.
var clientToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidateToolForClient lists why a strict MCP client would reject the tool in
// a tools/list result; it is empty for a tool that parses everywhere.
func ValidateToolForClient(t *mcpsdk.Tool) []string {
	var problems []string
	if t == nil {
		return []string{"nil tool"}
	}
	if !clientToolName.MatchString(t.Name) {
		problems = append(problems, fmt.Sprintf("name %q does not match [A-Za-z0-9_-]{1,64}", t.Name))
	}
	problems = append(problems, validateSchemaForClient("inputSchema", t.InputSchema, true)...)
	if t.OutputSchema != nil {
		problems = append(problems, validateSchemaForClient("outputSchema", t.OutputSchema, true)...)
	}
	return problems
}

func validateSchemaForClient(path string, schema any, root bool) []string {
	var m map[string]any
	switch v := schema.(type) {
	case map[string]any:
		m = v
	case json.RawMessage, []byte:
		var raw []byte
		if r, ok := v.(json.RawMessage); ok {
			raw = r
		} else {
			raw = v.([]byte)
		}
		if json.Unmarshal(raw, &m) != nil {
			return []string{path + ": not a JSON object"}
		}
	default:
		if root {
			// Typed schema structs marshal without null keywords.
			raw, err := json.Marshal(schema)
			if err != nil || json.Unmarshal(raw, &m) != nil || m == nil {
				return []string{path + ": not a JSON object"}
			}
		} else {
			return nil
		}
	}
	if m == nil {
		return []string{path + ": missing"}
	}
	var problems []string
	if root && m["type"] != "object" {
		problems = append(problems, path+`: root type must be "object"`)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := m[key]
		if value == nil && !nullValueKeywords[key] {
			problems = append(problems, fmt.Sprintf("%s.%s: null", path, key))
			continue
		}
		switch {
		case key == "required":
			if list, ok := value.([]any); !ok {
				if _, strs := value.([]string); !strs {
					problems = append(problems, path+".required: not an array")
				}
			} else {
				for _, item := range list {
					if _, ok := item.(string); !ok {
						problems = append(problems, path+".required: non-string entry")
						break
					}
				}
			}
		case schemaMapKeywords[key]:
			if sub, ok := value.(map[string]any); ok {
				names := make([]string, 0, len(sub))
				for n := range sub {
					names = append(names, n)
				}
				sort.Strings(names)
				for _, n := range names {
					problems = append(problems, validateChild(path+"."+key+"."+n, sub[n])...)
				}
			} else {
				problems = append(problems, fmt.Sprintf("%s.%s: not an object", path, key))
			}
		case schemaKeywords[key] || schemaListKeywords[key]:
			problems = append(problems, validateChild(path+"."+key, value)...)
		}
	}
	return problems
}

func validateChild(path string, value any) []string {
	switch v := value.(type) {
	case map[string]any:
		return validateSchemaForClient(path, v, false)
	case []any:
		var problems []string
		for i, item := range v {
			problems = append(problems, validateChild(fmt.Sprintf("%s[%d]", path, i), item)...)
		}
		return problems
	case nil:
		return []string{path + ": null"}
	}
	return nil
}
