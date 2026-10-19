package launch

import "strings"

// Correlation contains launcher-owned session facts, never ambient process or
// provider claims. An empty value removes an inherited claim.
type Correlation struct {
	TaskID, WorkstreamID, ProjectID string
}

func (c Correlation) values() map[string]string {
	return map[string]string{"TETHER_TASK_ID": c.TaskID, "TETHER_WORKSTREAM_ID": c.WorkstreamID, "TETHER_PROJECT_ID": c.ProjectID}
}

// SealMap copies env and replaces reserved correlation keys with session facts.
func (c Correlation) SealMap(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+3)
	for k, v := range env {
		out[k] = v
	}
	for k, v := range c.values() {
		delete(out, k)
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// SealEnv runs after provider/environment merging. Missing facts must not fall
// back to the daemon or Outpost process's own session correlation.
func (c Correlation) SealEnv(env []string) []string {
	values := c.values()
	out := make([]string, 0, len(env)+3)
	for _, v := range env {
		k, _, _ := strings.Cut(v, "=")
		if _, reserved := values[k]; !reserved {
			out = append(out, v)
		}
	}
	for _, k := range []string{"TETHER_TASK_ID", "TETHER_WORKSTREAM_ID", "TETHER_PROJECT_ID"} {
		if v := values[k]; v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// CorrelationFromEnv reads only the hub-supplied launch spec, before merging
// with an Outpost's ambient environment.
func CorrelationFromEnv(env map[string]string) Correlation {
	return Correlation{TaskID: env["TETHER_TASK_ID"], WorkstreamID: env["TETHER_WORKSTREAM_ID"], ProjectID: env["TETHER_PROJECT_ID"]}
}
