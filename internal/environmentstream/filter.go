package environmentstream

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/hollis-labs/tether/internal/identity"
)

// Filter is the server-side selection of GET /environment/events (CW-20261020-0017).
// Parameters combine with AND; repeated values of one parameter combine with OR.
// A filter only decides which events are written to a subscriber. It never
// changes a cursor: events that do not match are still consumed, so the stream
// advances its resume point and `synchronized` marker exactly as it does for an
// unfiltered stream, and gap handling is unaffected.
//
// Query parameters:
//
//	kind=<kind>            exact kind, or a prefix when it ends in *
//	category=<category>    class.category
//	subject=<prefix>       prefix of the event subject URN
//	app=<app>              producer app
//	severity_min=<level>   class.severity at or above debug|info|notice|warning|error|critical
//	scope.<dim>=<value>    task_id, project_id, workstream_id, session_id,
//	                       agent_urn, run_id, launch_id
type Filter struct {
	Kinds       []string
	Categories  []string
	Subjects    []string
	Apps        []string
	SeverityMin string
	Scope       map[string][]string
}

// ScopeDims are the scope.<dim> parameters a filter accepts.
var ScopeDims = []string{"task_id", "project_id", "workstream_id", "session_id", "agent_urn", "run_id", "launch_id"}

var severityRank = map[string]int{"debug": 0, "info": 1, "notice": 2, "warning": 3, "error": 4, "critical": 5}

// ParseFilter reads a Filter from query parameters. An unknown scope.<dim>, an
// unknown severity or an empty value is an error, so a typo is not a silent
// match-everything.
func ParseFilter(q url.Values) (Filter, error) {
	var f Filter
	var err error
	if f.Kinds, err = values(q, "kind"); err != nil {
		return f, err
	}
	if f.Categories, err = values(q, "category"); err != nil {
		return f, err
	}
	if f.Subjects, err = values(q, "subject"); err != nil {
		return f, err
	}
	if f.Apps, err = values(q, "app"); err != nil {
		return f, err
	}
	sev, err := values(q, "severity_min")
	if err != nil {
		return f, err
	}
	switch {
	case len(sev) > 1:
		return f, fmt.Errorf("severity_min may be given once")
	case len(sev) == 1:
		if _, ok := severityRank[sev[0]]; !ok {
			return f, fmt.Errorf("severity_min must be one of debug, info, notice, warning, error, critical")
		}
		f.SeverityMin = sev[0]
	}
	for key := range q {
		dim, ok := strings.CutPrefix(key, "scope.")
		if !ok {
			continue
		}
		known := false
		for _, d := range ScopeDims {
			known = known || d == dim
		}
		if !known {
			return f, fmt.Errorf("unknown filter scope.%s", dim)
		}
		vals, verr := values(q, key)
		if verr != nil {
			return f, verr
		}
		if f.Scope == nil {
			f.Scope = map[string][]string{}
		}
		f.Scope[dim] = vals
	}
	return f, nil
}

func values(q url.Values, key string) ([]string, error) {
	raw, ok := q[key]
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if v == "" {
			return nil, fmt.Errorf("%s must not be empty", key)
		}
		out = append(out, v)
	}
	return out, nil
}

// Empty reports whether the filter selects everything.
func (f Filter) Empty() bool {
	return len(f.Kinds) == 0 && len(f.Categories) == 0 && len(f.Subjects) == 0 && len(f.Apps) == 0 && f.SeverityMin == "" && len(f.Scope) == 0
}

// Match reports whether e passes the filter.
func (f Filter) Match(e Event) bool {
	if len(f.Kinds) > 0 && !anyMatch(f.Kinds, func(k string) bool {
		if p, ok := strings.CutSuffix(k, "*"); ok {
			return strings.HasPrefix(e.Kind, p)
		}
		return e.Kind == k
	}) {
		return false
	}
	category, severity := "", ""
	if e.Class != nil {
		category, severity = e.Class.Category, e.Class.Severity
	}
	if len(f.Categories) > 0 && !anyMatch(f.Categories, func(c string) bool { return c == category }) {
		return false
	}
	if len(f.Subjects) > 0 && !anyMatch(f.Subjects, func(p string) bool { return strings.HasPrefix(string(e.Subject), p) }) {
		return false
	}
	if len(f.Apps) > 0 && !anyMatch(f.Apps, func(a string) bool { return a == e.App }) {
		return false
	}
	if f.SeverityMin != "" {
		// An event with no severity is treated as info, the catalog default.
		rank, ok := severityRank[severity]
		if !ok {
			rank = severityRank["info"]
		}
		if rank < severityRank[f.SeverityMin] {
			return false
		}
	}
	for dim, want := range f.Scope {
		got := scopeValue(e, dim)
		if !anyMatch(want, func(v string) bool { return got != "" && v == got }) {
			return false
		}
	}
	return true
}

func scopeValue(e Event, dim string) string {
	if dim == "session_id" && e.SessionID != "" {
		return e.SessionID
	}
	s := e.Scope
	if s == nil {
		return ""
	}
	switch dim {
	case "task_id":
		return s.TaskID
	case "project_id":
		return s.ProjectID
	case "workstream_id":
		return s.WorkstreamID
	case "agent_urn":
		return s.AgentURN
	case "run_id":
		return s.RunID
	case "launch_id":
		return s.LaunchID
	}
	return ""
}

func anyMatch(vals []string, f func(string) bool) bool {
	for _, v := range vals {
		if f(v) {
			return true
		}
	}
	return false
}

// Visible reports whether a subscriber may receive an event with the given
// class.visibility. Only the real local operator credential sees "operator"
// events (identity.IsOperatorCredential: verified, operator kind and id, and not
// an issued token). A paired device, a service, an agent, or an operator-minted
// narrow token is not the operator, whatever scopes it carries, "*" included.
// A request with no principal at all (authentication off on the local socket,
// where there is no credential to check) is the local operator's own and is
// treated as one. All other visibility values are delivered as before.
func Visible(visibility string, p identity.Principal, hasPrincipal bool) bool {
	if visibility != "operator" || !hasPrincipal {
		return true
	}
	return identity.IsOperatorCredential(p)
}
