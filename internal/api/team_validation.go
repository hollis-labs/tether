package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/hollis-labs/tether/internal/teamsvc"
)

// teamFieldError contains request diagnostics only, never a host/service cause.
// These are safe to return consistently to HTTP, MCP and CLI callers.
type teamFieldError struct{ field, problem string }

func (e *teamFieldError) Error() string { return e.field + ": " + e.problem }
func (e *teamFieldError) Unwrap() error { return teamsvc.ErrInvalidRequest }

func invalidTeamField(field, problem string) error {
	if len(field) > 160 {
		field = field[:160] + "..."
	}
	// Quote unusual request-supplied names so control characters stay on one line.
	if strings.ContainsFunc(field, func(r rune) bool { return !unicode.IsPrint(r) }) {
		field = strconv.QuoteToASCII(field)
	}
	return &teamFieldError{field: field, problem: problem}
}

// checkTeamFields locates unknown fields, including their containing object or
// array index. encoding/json's unknown-field error carries only the leaf name.
// Types still decode through the standard strict decoder below.
func checkTeamFields(raw json.RawMessage, typ reflect.Type, path string) error {
	if string(raw) == "null" {
		return nil
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return invalidTeamField(path, "must be an object")
		}
		known := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if f.IsExported() && name != "-" {
				if name == "" {
					name = f.Name
				}
				known[name] = f.Type
			}
		}
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			fieldType, ok := known[name]
			if !ok {
				// Match encoding/json's existing case-insensitive field decoding.
				for canonical, candidate := range known {
					if strings.EqualFold(canonical, name) {
						fieldType, ok = candidate, true
						break
					}
				}
			}
			field := strings.TrimPrefix(path+"."+name, ".")
			if !ok {
				return invalidTeamField(field, "unknown field")
			}
			if err := checkTeamFields(fields[name], fieldType, field); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return invalidTeamField(path, "must be an array")
		}
		for i, value := range values {
			if err := checkTeamFields(value, typ.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	default:
		// Primitive values and map keys are checked by the strict JSON decoder.
	}
	return nil
}
