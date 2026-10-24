package mcpgateway

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// BriefDescriptionMax is the longest one-line description a search summary
// carries (CW-20261024-0050). The MCP client best-practices guidance for the
// catalog layer is names and one-line descriptions; the full text and schema
// come from detail=schema or tether_tool_list.
const BriefDescriptionMax = 160

// maxRequiredArgs bounds how many required-argument names a summary lists.
const maxRequiredArgs = 12

// abbreviations whose trailing period does not end a sentence.
var sentenceAbbreviations = map[string]bool{"e.g": true, "i.e": true, "vs": true, "etc": true, "cf": true, "approx": true}

// BriefDescription reduces a tool description to its first sentence, with
// whitespace collapsed. clean reports whether that sentence fit in
// BriefDescriptionMax; when it did not the text is cut at a word boundary and
// ends in an ellipsis, and the upstream should lead with a shorter sentence
// (the summary_too_long lint says so).
func BriefDescription(d string) (brief string, clean bool) {
	d = strings.TrimSpace(d)
	if d == "" {
		return "", true
	}
	if i := strings.Index(d, "\n\n"); i >= 0 {
		d = d[:i]
	}
	end := len(d)
	for i := 0; i < len(d); i++ {
		c := d[i]
		if c != '.' && c != '!' && c != '?' {
			continue
		}
		if i+1 < len(d) && d[i+1] != ' ' && d[i+1] != '\n' && d[i+1] != '\t' {
			continue // 'v0.1', 'a.b', a decimal: not a sentence end
		}
		if c == '.' {
			start := strings.LastIndexAny(d[:i], " \n\t(") + 1
			if sentenceAbbreviations[strings.ToLower(d[start:i])] {
				continue
			}
		}
		end = i + 1
		break
	}
	brief = strings.Join(strings.Fields(d[:end]), " ")
	if utf8.RuneCountInString(brief) <= BriefDescriptionMax {
		return brief, true
	}
	runes := []rune(brief)[:BriefDescriptionMax-1]
	cut := string(runes)
	if i := strings.LastIndex(cut, " "); i > BriefDescriptionMax-40 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:-") + "…", false
}

// requiredArgs lists the top-level required argument names of an input schema,
// in the order the schema declares them, bounded by maxRequiredArgs. Anything it
// cannot read yields nil: a summary then simply omits the field.
func requiredArgs(schema any) []string {
	var generic map[string]any
	switch v := schema.(type) {
	case nil:
		return nil
	case map[string]any:
		generic = v
	default:
		raw, err := json.Marshal(v)
		if err != nil || json.Unmarshal(raw, &generic) != nil {
			return nil
		}
	}
	var names []string
	switch req := generic["required"].(type) {
	case []any:
		for _, item := range req {
			if s, ok := item.(string); ok && s != "" {
				names = append(names, s)
			}
		}
	case []string:
		names = append(names, req...)
	}
	if len(names) > maxRequiredArgs {
		names = names[:maxRequiredArgs]
	}
	return names
}
