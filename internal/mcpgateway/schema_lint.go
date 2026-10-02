package mcpgateway

import (
	"encoding/json"
	"errors"
	"net/url"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

// Decode a copy: resolving a schema must not modify authored definitions, fetch
// URLs, or include schema contents (which can contain secrets) in findings.
func lintInputSchema(value any) string {
	budget := schemaBudget{}
	if !budget.visit(reflect.ValueOf(value), 0) {
		return "input_schema_limit"
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "input_schema"
	}
	if len(raw) > 256*1024 {
		return "input_schema_limit"
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil || schema.Type != "object" {
		return "input_schema"
	}
	if !(&schemaBudget{}).visit(reflect.ValueOf(&schema), 0) {
		return "input_schema_limit"
	}
	if !validSchemaKeywords(reflect.ValueOf(&schema)) {
		return "input_schema"
	}
	external := errors.New("external schema reference not examined")
	_, err = schema.Resolve(&jsonschema.ResolveOptions{Loader: func(*url.URL) (*jsonschema.Schema, error) { return nil, external }})
	if errors.Is(err, external) {
		return "input_schema_unexamined"
	}
	if err != nil {
		return "input_schema"
	}
	return ""
}

// The SDK resolver checks references and regular expressions, but not all
// keyword constraints. Check basic vocabulary invariants on schema nodes only;
// defaults/examples/extensions remain data, not schemas. No instance validation.
func validSchemaKeywords(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return validSchemaKeywords(v.Elem())
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[jsonschema.Schema]() {
			s := v.Interface().(jsonschema.Schema)
			allowed := func(t string) bool {
				return t == "" || t == "object" || t == "array" || t == "string" || t == "number" || t == "integer" || t == "boolean" || t == "null"
			}
			if !allowed(s.Type) {
				return false
			}
			seen := map[string]bool{}
			for _, t := range s.Types {
				if t == "" || !allowed(t) || seen[t] {
					return false
				}
				seen[t] = true
			}
			if s.Types != nil && len(s.Types) == 0 {
				return false
			}
			for _, bound := range []*int{s.MinLength, s.MaxLength, s.MinItems, s.MaxItems, s.MinContains, s.MaxContains, s.MinProperties, s.MaxProperties} {
				if bound != nil && *bound < 0 {
					return false
				}
			}
			if s.MultipleOf != nil && *s.MultipleOf <= 0 {
				return false
			}
			for _, group := range [][]*jsonschema.Schema{s.AllOf, s.AnyOf, s.OneOf} {
				if group != nil && len(group) == 0 {
					return false
				}
			}
			if s.Enum != nil && len(s.Enum) == 0 {
				return false
			}
			unique := func(values []string) bool {
				seen := map[string]bool{}
				for _, x := range values {
					if seen[x] {
						return false
					}
					seen[x] = true
				}
				return true
			}
			if !unique(s.Required) {
				return false
			}
			for _, values := range s.DependentRequired {
				if !unique(values) {
					return false
				}
			}
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() && !validSchemaKeywords(v.Field(i)) {
				return false
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if !validSchemaKeywords(iter.Value()) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if !validSchemaKeywords(v.Index(i)) {
				return false
			}
		}
	default: // Scalar values are leaves.
		return true
	}
	return true
}

// Preflight before encoding/decoding/resolution. It also terminates cyclic Go
// object graphs; JSON reference cycles are handled by the schema resolver.
// Values received over MCP are maps/scalars. Typed SDK schemas use this same
// bounded traversal, so linting either representation has the same limits.
type schemaBudget struct{ nodes, bytes int }

func (b *schemaBudget) visit(v reflect.Value, depth int) bool {
	b.nodes++
	if b.nodes > 4096 || depth > 64 || b.bytes > 256*1024 {
		return false
	}
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if !v.IsNil() {
			return b.visit(v.Elem(), depth+1)
		}
	case reflect.String:
		b.bytes += v.Len()
	case reflect.Map:
		if v.Len() > 4096 {
			return false
		}
		iter := v.MapRange()
		for iter.Next() {
			if !b.visit(iter.Key(), depth+1) || !b.visit(iter.Value(), depth+1) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Len() > 4096 {
			return false
		}
		for i := 0; i < v.Len(); i++ {
			if !b.visit(v.Index(i), depth+1) {
				return false
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() && !v.Field(i).IsZero() && !b.visit(v.Field(i), depth+1) {
				return false
			}
		}
	default: // Scalar values are leaves.
		return b.bytes <= 256*1024
	}
	return b.bytes <= 256*1024
}
