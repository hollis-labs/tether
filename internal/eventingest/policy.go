package eventingest

import (
	"encoding/json"
	"fmt"
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
// That fallback is a DEV policy, to be revisited at release posture.
//
// A schema that is declared but cannot be read, parsed or compiled is a
// different thing: it is a broken policy, never a reason to degrade. newPolicies
// builds every catalog kind's sanitizer up front and returns an error naming the
// kind and schema, so the daemon does not enable ingest on a broken policy. If a
// kind still cannot be resolved at ingest time, forKind reports the error and the
// payload is not stored (see Service.sanitize).
type policies struct {
	mu   sync.Mutex
	kind map[string]*redact.Sanitizer
	deny *redact.Sanitizer
}

// Seams for tests: the catalog schema source and the allowlist compiler.
var (
	payloadSchemaFor = catalog.PayloadSchema
	newAllowlist     = func(paths []string) (*redact.Sanitizer, error) {
		return redact.NewSanitizer(redact.Policy{Mode: redact.Allowlist, AllowedPaths: paths, Sensitivity: redact.Private, MaxBytes: ingest.MaxPayloadBytes})
	}
)

func newPolicies() (*policies, error) {
	deny, err := redact.NewSanitizer(redact.Policy{Mode: redact.Denylist, Sensitivity: redact.Private, MaxBytes: ingest.MaxPayloadBytes})
	if err != nil {
		return nil, err
	}
	p := &policies{kind: map[string]*redact.Sanitizer{}, deny: deny}
	cat, err := catalog.Load()
	if err != nil {
		return nil, fmt.Errorf("event ingest: event catalog: %w", err)
	}
	for _, e := range cat.Kinds {
		if _, _, err := p.resolve(e.Kind); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// forKind returns the sanitizer for kind and whether it is an allowlist. An
// error means the kind's policy is broken and the payload must not be stored.
func (p *policies) forKind(kind string) (*redact.Sanitizer, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resolve(kind)
}

// resolve is forKind with p.mu held (or, in newPolicies, before p is shared).
func (p *policies) resolve(kind string) (*redact.Sanitizer, bool, error) {
	if s, ok := p.kind[kind]; ok {
		return s, s != p.deny, nil
	}
	paths, ok, err := allowedPathsFor(kind)
	if err != nil {
		return nil, false, err
	}
	s := p.deny
	if ok {
		a, err := newAllowlist(paths)
		if err != nil {
			return nil, false, fmt.Errorf("event ingest: payload policy for kind %q does not compile: %w", kind, err)
		}
		s = a
	}
	p.kind[kind] = s
	return s, s != p.deny, nil
}

// allowedPaths derives the RFC 6901 allowlist from the kind's catalog payload
// schema. ok is false when the kind has no schema or the schema cannot be
// reduced to a path list (the denylist case). A broken schema is not that case.
func allowedPaths(kind string) ([]string, bool) {
	paths, ok, err := allowedPathsFor(kind)
	return paths, ok && err == nil
}

func allowedPathsFor(kind string) ([]string, bool, error) {
	e, found := catalog.Lookup(kind)
	if !found || e.PayloadSchema == "" {
		return nil, false, nil
	}
	raw, err := payloadSchemaFor(e.PayloadSchema)
	if err != nil {
		return nil, false, fmt.Errorf("event ingest: payload schema %q for kind %q cannot be read: %w", e.PayloadSchema, kind, err)
	}
	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, false, fmt.Errorf("event ingest: payload schema %q for kind %q is not valid JSON: %w", e.PayloadSchema, kind, err)
	}
	set := map[string]bool{}
	if !walkSchema(schema, "", set) || len(set) == 0 {
		return nil, false, nil
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, true, nil
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
