package workerhub

import "strings"

// LaunchFailureCause reduces a worker launch error to one bounded line safe to record and show a caller.
func LaunchFailureCause(err error) string {
	cause := strings.Join(strings.Fields(err.Error()), " ")
	if r := []rune(cause); len(r) > 300 {
		cause = string(r[:300]) + "..."
	}
	return cause
}
