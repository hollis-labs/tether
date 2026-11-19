package agystdio

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Provider user-input echoes are not assistant output. Unknown frames can also
// echo the rendered input, so remove its private app-context block before any
// decoder activity/raw event can retain it.
func outputFrame(raw []byte, block string) []byte {
	var h struct {
		Event string `json:"event"`
		Step  struct {
			Type string `json:"step_type"`
		} `json:"step_update"`
	}
	_ = json.Unmarshal(raw, &h)
	if h.Event == "user_input" || h.Event == "user" || (h.Event == "step_update" && h.Step.Type == "user_input") {
		return nil
	}
	if block == "" {
		return raw
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		if bytes.Contains(raw, []byte("app-context")) {
			return nil
		}
		return raw
	}
	var redact func(any) any
	redact = func(v any) any {
		switch x := v.(type) {
		case string:
			x = strings.ReplaceAll(x, block, "[app context omitted]")
			if start := strings.Index(x, "<app-context>"); start >= 0 {
				end := strings.LastIndex(x, "</app-context>")
				if end < start {
					return x[:start] + "[app context omitted]"
				}
				return x[:start] + "[app context omitted]" + x[end+len("</app-context>"):]
			}
			return x
		case []any:
			for i, v := range x {
				x[i] = redact(v)
			}
		case map[string]any:
			for k, v := range x {
				x[k] = redact(v)
			}
		}
		return v
	}
	cleaned, err := json.Marshal(redact(value))
	if err != nil {
		return nil
	}
	return cleaned
}
