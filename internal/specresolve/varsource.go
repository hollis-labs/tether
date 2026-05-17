package specresolve

import (
	"fmt"
	"regexp"

	"github.com/hollis-labs/go-agent-launch/agentlaunch"

	"github.com/hollis-labs/tether/internal/config"
)

// inputTagPattern matches an {{ inputs.<name> }} merge tag. The S5
// launch-spec corpus parameterizes its var sources (file paths, cmd argv,
// call targets/args) with these tags so one LaunchSpec serves every
// project. The S4.2 VarResolver does NOT itself expand merge tags inside a
// var source — it resolves the source verbatim — so the Resolver must
// expand them first, against the bag's resolved inputs.
//
// Only the inputs scope is matched: var sources reference inputs, never
// other vars (which would be a resolution-order cycle). Whitespace inside
// the braces is tolerated, mirroring the template engine.
var inputTagPattern = regexp.MustCompile(`\{\{\s*inputs\.([A-Za-z0-9_-]+)\s*\}\}`)

// expandVarSources returns a copy of spec.Vars with every {{ inputs.* }}
// merge tag in every var source expanded against inputs. The original
// VarSpec slice is not mutated; the pointer-typed source branches
// (VarFileRef / VarCallRef / VarCmdRef) are deep-copied so the corpus spec
// stays reusable across launches.
//
// inputs is the resolved-input map from LaunchSpec.Render — the effective
// values after defaults are applied. A tag referencing an input absent
// from the map expands to the empty string (the same lenient behavior the
// template engine uses); a missing required input is already caught by
// Render's Missing list, so this never silently hides one.
func expandVarSources(vars []agentlaunch.VarSpec, inputs map[string]any) []agentlaunch.VarSpec {
	out := make([]agentlaunch.VarSpec, len(vars))
	for i := range vars {
		v := vars[i]
		v.Source = expandSource(v.Source, inputs)
		out[i] = v
	}
	return out
}

// expandSource expands the merge tags inside one VarSource, returning a
// copy with fresh pointer branches.
func expandSource(src agentlaunch.VarSource, inputs map[string]any) agentlaunch.VarSource {
	switch src.Kind {
	case agentlaunch.VarSourceLiteral:
		// a literal source has no path/target/argv — nothing to expand
	case agentlaunch.VarSourceFile:
		if src.File != nil {
			f := *src.File
			f.Path = config.Expand(expandInputTags(f.Path, inputs))
			src.File = &f
		}
	case agentlaunch.VarSourceCall:
		if src.Call != nil {
			c := *src.Call
			c.Target = expandInputTags(c.Target, inputs)
			if c.Args != nil {
				args := make(map[string]any, len(c.Args))
				for k, val := range c.Args {
					if s, ok := val.(string); ok {
						args[k] = expandInputTags(s, inputs)
					} else {
						args[k] = val
					}
				}
				c.Args = args
			}
			src.Call = &c
		}
	case agentlaunch.VarSourceCmd:
		if src.Cmd != nil {
			cmd := *src.Cmd
			argv := make([]string, len(cmd.Argv))
			for i, a := range cmd.Argv {
				argv[i] = expandInputTags(a, inputs)
			}
			cmd.Argv = argv
			cmd.Workdir = config.Expand(expandInputTags(cmd.Workdir, inputs))
			src.Cmd = &cmd
		}
	}
	return src
}

// expandInputTags substitutes every {{ inputs.<name> }} tag in s with the
// stringified resolved-input value. An unknown input expands to "".
func expandInputTags(s string, inputs map[string]any) string {
	if s == "" {
		return s
	}
	return inputTagPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := inputTagPattern.FindStringSubmatch(match)[1]
		if v, ok := inputs[name]; ok && v != nil {
			return fmt.Sprintf("%v", v)
		}
		return ""
	})
}
