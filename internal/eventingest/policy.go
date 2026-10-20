package eventingest

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"
	"github.com/hollis-labs/libs/security/redact"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/ingest"
)

// policies gives each ingested kind the sanitizer for its payload.
//
// A kind whose catalog payload schema is a plain tree of declared properties
// gets an allowlist: only the declared paths are kept, everything else is
// removed (fail closed), so a producer cannot smuggle a field into the log by
// adding it. A kind with no schema, a free-form object, or a schema that uses
// composition (oneOf, $ref, ...) cannot be reduced to a path list, so it gets
// the denylist: the redaction engine scrubs secrets and PII but keeps the shape.
type policies struct {
	mu   sync.Mutex
	kind map[string]*redact.Sanitizer
	deny *redact.Sanitizer
}

func newPolicies() (*policies, error) {
	deny, err := redact.NewSanitizer(redact.Policy{Mode: redact.Denylist, Sensitivity: redact.Private, MaxBytes: ingest.MaxPayloadBytes})
	if err != nil {
		return nil, err
	}
	return &policies{kind: map[string]*redact.Sanitizer{}, deny: deny}, nil
}

// forKind returns the sanitizer for kind and whether it is an allowlist.
func (p *policies) forKind(kind string) (*redact.Sanitizer, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.kind[kind]; ok {
		return s, s != p.deny
	}
	s := p.deny
	if paths, ok := allowedPaths(kind); ok {
		if a, err := redact.NewSanitizer(redact.Policy{Mode: redact.Allowlist, AllowedPaths: paths, Sensitivity: redact.Private, MaxBytes: ingest.MaxPayloadBytes}); err == nil {
			s = a
		}
	}
	p.kind[kind] = s
	return s, s != p.deny
}

// allowedPaths derives the RFC 6901 allowlist from the kind's catalog payload
// schema. ok is false when the schema cannot be reduced to a path list.
func allowedPaths(kind string) ([]string, bool) {
	e, found := catalog.Lookup(kind)
	if !found || e.PayloadSchema == "" {
		return nil, false
	}
	raw, err := catalog.PayloadSchema(e.PayloadSchema)
	if err != nil {
		return nil, false
	}
	var schema any
	if json.Unmarshal(raw, &schema) != nil {
		return nil, false
	}
	set := map[string]bool{}
	if !walkSchema(schema, "", set) || len(set) == 0 {
		return nil, false
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, true
}

// walkSchema adds the leaf paths of a declared-properties schema. It returns
// false for anything it cannot express as a closed path list.
func walkSchema(n any, ptr string, out map[string]bool) bool {
	m, ok := n.(map[string]any)
	if !ok {
		return false // a boolean schema accepts anything
	}
	for _, k := range []string{"$ref", "oneOf", "anyOf", "allOf", "not", "if", "patternProperties"} {
		if _, has := m[k]; has {
			return false
		}
	}
	if props, has := m["properties"].(map[string]any); has {
		for name, sub := range props {
			if !walkSchema(sub, ptr+"/"+escapePointer(name), out) {
				return false
			}
		}
		return true
	}
	switch t, _ := m["type"].(string); t {
	case "object":
		return false // free-form object: no declared members to allow
	case "array":
		items, has := m["items"]
		if !has {
			return false
		}
		return walkSchema(items, ptr+"/*", out)
	case "":
		if _, isEnum := m["enum"]; !isEnum {
			if _, isConst := m["const"]; !isConst {
				return false // no type, enum or const: anything
			}
		}
	}
	if ptr == "" {
		return false
	}
	out[ptr] = true
	return true
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
