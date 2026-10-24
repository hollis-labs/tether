package mcpgateway

import (
	"encoding/json"
	"sort"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ArgHint is the compact correction the gateway attaches to a tool error when
// the arguments sent do not fit the tool's own schema (CW-20261024-0051). In
// search mode a caller often calls a tool by a name it remembers, with no schema
// in context, so the error is the only thing that can teach it the contract.
//
// It is derived only from the target's advertised schema and the arguments that
// were sent, never from an upstream's error wording, and it is attached only to
// a call that already failed: a successful call is never touched.
type ArgHint struct {
	Tool string `json:"tool"`
	// UnknownArgs were sent but are not declared by the tool.
	UnknownArgs []string `json:"unknown_args,omitempty"`
	// DidYouMean maps a sent name to the declared name it most likely meant.
	DidYouMean map[string]string `json:"did_you_mean,omitempty"`
	// MissingRequired are declared-required names that were not sent.
	MissingRequired []string `json:"missing_required,omitempty"`
	RequiredArgs    []string `json:"required_args,omitempty"`
	// AcceptedArgs is listed only when the list is short; otherwise Next points
	// at tether_tool_list for the full schema (the list costs tokens).
	AcceptedArgs []string `json:"accepted_args,omitempty"`
	// Example is a minimal call built from the required arguments, with placeholder
	// values; it shows the shape, not real data.
	Example map[string]any `json:"example,omitempty"`
	Next    string         `json:"next,omitempty"`
}

const (
	maxAcceptedListed = 15
	maxExampleArgs    = 8
)

type declared struct {
	order    []string // declaration order is not available from a map; sorted for stability
	props    map[string]map[string]any
	required []string
	open     bool // additionalProperties is true or a schema: unknown names are legitimate
}

func readDeclared(schema any) declared {
	var generic map[string]any
	switch v := schema.(type) {
	case nil:
		return declared{}
	case map[string]any:
		generic = v
	default:
		raw, err := json.Marshal(v)
		if err != nil || json.Unmarshal(raw, &generic) != nil {
			return declared{}
		}
	}
	d := declared{props: map[string]map[string]any{}, required: requiredArgs(generic)}
	if props, ok := generic["properties"].(map[string]any); ok {
		for name, p := range props {
			prop, _ := p.(map[string]any)
			d.props[name] = prop
			d.order = append(d.order, name)
		}
	}
	sort.Strings(d.order)
	switch ap := generic["additionalProperties"].(type) {
	case bool:
		d.open = ap
	case map[string]any:
		d.open = true
	}
	return d
}

// BuildArgHint compares the arguments sent with the tool's schema. It returns
// nil when they fit (so the failure is not an argument-contract problem) or when
// the schema declares nothing to compare against.
func BuildArgHint(tool *mcpsdk.Tool, args map[string]any) *ArgHint {
	if tool == nil {
		return nil
	}
	d := readDeclared(tool.InputSchema)
	if len(d.props) == 0 && len(d.required) == 0 {
		return nil
	}
	var unknown, missing []string
	if !d.open {
		for name := range args {
			if strings.HasPrefix(name, "_") {
				continue // gateway transport metadata, never a caller argument
			}
			if _, ok := d.props[name]; !ok {
				unknown = append(unknown, name)
			}
		}
		sort.Strings(unknown)
	}
	for _, name := range d.required {
		if _, sent := args[name]; !sent {
			missing = append(missing, name)
		}
	}
	if len(unknown) == 0 && len(missing) == 0 {
		return nil
	}
	hint := &ArgHint{Tool: tool.Name, UnknownArgs: unknown, MissingRequired: missing, RequiredArgs: d.required}
	hint.DidYouMean = suggestNames(unknown, d, args, missing)
	if len(d.props) > 0 && len(d.props) <= maxAcceptedListed {
		hint.AcceptedArgs = append([]string(nil), d.order...)
	} else if len(d.props) > maxAcceptedListed {
		hint.Next = `call tether_tool_list with {"names":["` + tool.Name + `"]} for the full schema`
	}
	hint.Example = exampleFor(d)
	return hint
}

// suggestNames pairs each unknown name with the declared name it most likely
// meant. Each declared name is used at most once and never one already sent.
func suggestNames(unknown []string, d declared, args map[string]any, missing []string) map[string]string {
	if len(unknown) == 0 {
		return nil
	}
	isMissing := map[string]bool{}
	for _, m := range missing {
		isMissing[m] = true
	}
	var candidates []string
	for _, name := range d.order {
		if _, sent := args[name]; !sent {
			candidates = append(candidates, name)
		}
	}
	out := map[string]string{}
	used := map[string]bool{}
	for _, u := range unknown {
		best, bestScore := "", 0.0
		for _, c := range candidates {
			if used[c] {
				continue
			}
			score := nameSimilarity(u, c)
			if isMissing[c] {
				score += 0.05 // a required, unsent name is the likelier intent
			}
			if score > bestScore {
				best, bestScore = c, score
			}
		}
		if bestScore >= 0.5 {
			out[u] = best
			used[best] = true
		}
	}
	// One unknown and one missing required name with nothing lexical to go on is
	// still a strong signal (the caller sent the right value under the wrong name).
	if len(out) == 0 && len(unknown) == 1 && len(missing) == 1 && !used[missing[0]] {
		out[unknown[0]] = missing[0]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// synonymGroups holds argument names that callers use interchangeably for the
// same thing. Deliberately tiny and generic: only confusions seen repeatedly in
// real tool-call errors (body vs content), never anything specific to one app.
var synonymGroups = [][]string{
	{"body", "content", "text", "message", "msg"},
	{"query", "q", "search", "term"},
}

func sameSynonymGroup(a, b string) bool {
	for _, group := range synonymGroups {
		var hasA, hasB bool
		for _, name := range group {
			hasA = hasA || name == a
			hasB = hasB || name == b
		}
		if hasA && hasB {
			return true
		}
	}
	return false
}

func splitName(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == '_' || r == '-' || r == '.' })
}

// nameSimilarity scores two argument names in [0,1]: equal ignoring separators
// and case, containment, shared tokens (a shared trailing token such as "id"
// counts extra), and edit distance.
func nameSimilarity(a, b string) float64 {
	na, nb := strings.Join(splitName(a), ""), strings.Join(splitName(b), "")
	if na == nb {
		return 1
	}
	best := 0.0
	if len(na) >= 4 && len(nb) >= 4 && (strings.Contains(na, nb) || strings.Contains(nb, na)) {
		best = 0.8
	}
	if sameSynonymGroup(na, nb) {
		best = 0.7
	}
	ta, tb := splitName(a), splitName(b)
	if len(ta) == 0 || len(tb) == 0 {
		return best // a name made only of separators has no tokens to compare
	}
	// A bare token against a name that ends in it: "id" vs "item_id".
	if (len(ta) == 1 || len(tb) == 1) && ta[len(ta)-1] == tb[len(tb)-1] && best < 0.55 {
		best = 0.55
	}
	set := map[string]bool{}
	for _, t := range ta {
		set[t] = true
	}
	inter, union := 0, len(set)
	seen := map[string]bool{}
	for _, t := range tb {
		if seen[t] {
			continue
		}
		seen[t] = true
		if set[t] {
			inter++
		} else {
			union++
		}
	}
	if union > 0 {
		score := 0.5 * float64(inter) / float64(union)
		if len(ta) > 1 && len(tb) > 1 && ta[len(ta)-1] == tb[len(tb)-1] {
			score += 0.3
		}
		if score > best {
			best = score
		}
	}
	maxLen := max(len(na), len(nb))
	if maxLen > 0 {
		if ratio := 1 - float64(editDistance(na, nb))/float64(maxLen); ratio >= 0.6 && ratio > best {
			best = ratio
		}
	}
	return best
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// exampleFor builds a minimal call from the required arguments, using an enum's
// first value, a default, a schema example, or a typed placeholder. When the
// schema itself carries top-level examples the first object one is used instead.
func exampleFor(d declared) map[string]any {
	if len(d.required) == 0 {
		return nil
	}
	out := map[string]any{}
	for i, name := range d.required {
		if i == maxExampleArgs {
			break
		}
		out[name] = placeholder(name, d.props[name])
	}
	return out
}

func placeholder(name string, prop map[string]any) any {
	if prop == nil {
		return "<" + name + ">"
	}
	if enum, ok := prop["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	if v, ok := prop["default"]; ok && v != nil {
		return v
	}
	if ex, ok := prop["examples"].([]any); ok && len(ex) > 0 {
		return ex[0]
	}
	typ, _ := prop["type"].(string)
	if typ == "" {
		if list, ok := prop["type"].([]any); ok && len(list) > 0 {
			typ, _ = list[0].(string)
		}
	}
	switch typ {
	case "integer", "number":
		return 0
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	}
	return "<" + name + ">"
}

// SynthesizedExample is the minimal valid-shaped call for a tool, for discovery
// responses. The bool reports whether the example came from the schema's own
// examples (true) or was synthesized from required arguments (false).
func SynthesizedExample(schema any) (map[string]any, bool) {
	var generic map[string]any
	switch v := schema.(type) {
	case map[string]any:
		generic = v
	default:
		raw, err := json.Marshal(v)
		if err != nil || json.Unmarshal(raw, &generic) != nil {
			return nil, false
		}
	}
	if ex, ok := generic["examples"].([]any); ok {
		for _, e := range ex {
			if obj, ok := e.(map[string]any); ok && len(obj) > 0 {
				return obj, true
			}
		}
	}
	return exampleFor(readDeclared(generic)), false
}
