//go:build !windows

package shimcodex

import (
	"bytes"
	"encoding/json"
	"io"
)

// A peer must not interpret duplicate parameter keys differently from the
// local authority check. Bound every nesting level before method admission.
func uniqueJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok || seen[name] || len(seen) >= 256 {
					return false
				}
				seen[name] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !value(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func validateParams(method string, raw json.RawMessage) error {
	if !uniqueJSON(raw) {
		return fail("invalid_params")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return fail("invalid_params")
	}
	var allowed []string
	switch method {
	case "initialize":
		allowed = []string{"clientInfo", "capabilities"}
	case "initialized":
		allowed = nil
	case "thread/start":
		allowed = []string{"cwd"}
	case "turn/start":
		allowed = []string{"threadId", "input"}
	case "turn/steer":
		allowed = []string{"threadId", "expectedTurnId", "input"}
	case "turn/interrupt":
		allowed = []string{"threadId", "turnId"}
	default:
		return fail("method_unsupported")
	}
	for name := range fields {
		ok := false
		for _, candidate := range allowed {
			if name == candidate {
				ok = true
				break
			}
		}
		if !ok {
			return fail("params_unsupported")
		}
	}
	if input, ok := fields["input"]; ok {
		var items []map[string]json.RawMessage
		if json.Unmarshal(input, &items) != nil || items == nil {
			return fail("invalid_params")
		}
		for _, item := range items {
			for name := range item {
				if name != "type" && name != "text" {
					return fail("params_unsupported")
				}
			}
			var kind, text string
			if json.Unmarshal(item["type"], &kind) != nil || json.Unmarshal(item["text"], &text) != nil || kind != "text" {
				return fail("params_unsupported")
			}
		}
	}
	return nil
}
