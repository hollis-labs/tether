package api

import (
	"net/http"
	"regexp"

	"github.com/hollis-labs/tether/internal/identity"
)

// taskIDPattern bounds the launch-time task stamp (CW-20261012-0062a). The value is the
// bare Torque id (CW-20261016-0155) and is never transformed; the pattern only keeps
// control characters, whitespace and path or shell syntax out of a value that is planted
// in the agent's environment and stored on the session row. There is no existence check
// against Torque.
var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// taskStampLauncher reports whether the caller may assert a task id on a launch or a
// bootstrap. Only a launcher may: a request with no principal (the local socket, trusted
// as for the rest of the local API) or a verified principal that is not a session or an
// agent (the operator, a Torque service or device credential). An agent, a session
// credential or an unverified claim can never set it.
func taskStampLauncher(r *http.Request) bool {
	p, ok := identity.FromContext(r.Context())
	if !ok {
		return true
	}
	if p.Kind == "session" || p.Kind == "agent" || p.SessionID != "" {
		return false
	}
	return p.Verified
}

// checkTaskStamp validates a task id asserted on a request. An empty id is always fine.
// A non-empty id from a non-launcher is refused with 403 task_id_not_permitted, never
// silently dropped, so a misbehaving caller finds out; a malformed id is a 400.
func checkTaskStamp(w http.ResponseWriter, r *http.Request, taskID string) bool {
	if taskID == "" {
		return true
	}
	if !taskStampLauncher(r) {
		writeError(w, http.StatusForbidden, CodeForbidden, "task_id_not_permitted: only the launcher may assert a task id")
		return false
	}
	if !taskIDPattern.MatchString(taskID) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "task_id must be a bare task id (letters, digits, '.', '_' or '-', at most 64 characters)")
		return false
	}
	return true
}
