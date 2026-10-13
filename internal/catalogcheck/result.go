// Package catalogcheck holds the catalog-structural checks `tether doctor`
// runs, so the daemon and MCP tools can run the same ones on the live catalog
// or on a draft (CW-20261013-0003). It checks the files on disk: parsing,
// references between agents, launches, projects and providers, sandbox
// profiles, MCP server and profile definitions, MCP grants and role modules.
// It does not probe the host (daemon reachability, migrations, provider
// binaries, state and log directories); those stay in the CLI.
package catalogcheck

import (
	"regexp"
	"sort"
)

// Status values.
const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
)

// Result is one check outcome. Its JSON is `tether doctor --json`'s shape;
// Entity is added when a finding names a catalog entity.
type Result struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // "ok", "warn", "fail"
	Message string `json:"message"`
	Remedy  string `json:"remedy,omitempty"`
	// Entity is "<kind>:<id>" (agent, launch, project, provider, skill,
	// server, profile) when the finding is about one.
	Entity string `json:"entity,omitempty"`
}

// OK returns a passing result.
func OK(name, msg string) Result { return Result{Name: name, Status: StatusOK, Message: msg} }

// Warn returns a warning.
func Warn(name, msg, remedy string) Result {
	return Result{Name: name, Status: StatusWarn, Message: msg, Remedy: remedy, Entity: entityOf(msg)}
}

// Fail returns a failure.
func Fail(name, msg, remedy string) Result {
	return Result{Name: name, Status: StatusFail, Message: msg, Remedy: remedy, Entity: entityOf(msg)}
}

var entityRe = regexp.MustCompile(`\b(agent|launch|project|provider|skill|server|profile|route|channel|worker)\s+"([^"]+)"`)

// entityOf finds the first `<kind> "<id>"` a validation message names.
func entityOf(msg string) string {
	if m := entityRe.FindStringSubmatch(msg); m != nil {
		return m[1] + ":" + m[2]
	}
	return ""
}

// Report is the outcome of Run.
type Report struct {
	// OK is false when any result is a failure.
	OK       bool     `json:"ok"`
	Failures int      `json:"failures"`
	Warnings int      `json:"warnings"`
	Results  []Result `json:"results"`
}

func newReport(results []Result) Report {
	r := Report{OK: true, Results: results}
	for _, c := range results {
		switch c.Status {
		case StatusFail:
			r.Failures++
			r.OK = false
		case StatusWarn:
			r.Warnings++
		}
	}
	sort.SliceStable(r.Results, func(i, j int) bool { return rank(r.Results[i].Status) < rank(r.Results[j].Status) })
	return r
}

func rank(s string) int {
	switch s {
	case StatusFail:
		return 0
	case StatusWarn:
		return 1
	}
	return 2
}
