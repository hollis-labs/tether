package api

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

// taskIDPattern bounds the launch-time task stamp (CW-20261012-0062a). The value is the
// bare Torque id (CW-20261016-0155) and is never transformed; the pattern only keeps
// control characters, whitespace and path or shell syntax out of a value that is planted
// in the agent's environment and stored on the session row. There is no existence check
// against Torque.
var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// eventTaskIDDropped is the identity_audit event for a task_id a non-operator sent and
// the launch discarded.
const eventTaskIDDropped = "task_id_dropped"

// taskStamp decides what a request's task_id does (architect ruling for tether#379):
//
//   - the verified local operator: accepted (400 if malformed);
//   - an agent or a session credential: refused with 403 task_id_not_permitted;
//   - any other caller (worker device, other service, unverified claim, no principal):
//     the field is dropped, the request proceeds without a stamp, and the drop is
//     recorded in identity_audit.
//
// The operator check is an explicit allow, not "anything that is not an agent", so a new
// principal kind starts denied. It returns the task id to use ("" when none or dropped)
// and false after writing an error response.
//
// TODO(CW-20261012-0062): add a Torque service principal after the issuer lands
// (CW-20261016-0017) and allow it here; decision tether_task_id_stamp_and_usage_rows_2026_10_16.
func (s *Server) taskStamp(w http.ResponseWriter, r *http.Request, taskID string) (string, bool) {
	if taskID == "" {
		return "", true
	}
	p, ok := identity.FromContext(r.Context())
	if ok && (p.Kind == "session" || p.Kind == "agent" || p.SessionID != "") {
		writeError(w, http.StatusForbidden, CodeForbidden, "task_id_not_permitted: an agent or session may not assert a task id")
		return "", false
	}
	if !ok || !identity.IsOperatorCredential(p) {
		if ok {
			s.auditTaskIDDropped(r, p)
		}
		return "", true
	}
	if !taskIDPattern.MatchString(taskID) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "task_id must be a bare task id (letters, digits, '.', '_' or '-', at most 64 characters)")
		return "", false
	}
	return taskID, true
}

// auditTaskIDDropped records the dropped field with the caller's real authentication state.
func (s *Server) auditTaskIDDropped(r *http.Request, p identity.Principal) {
	if s.IdentityAudit == nil {
		return
	}
	auth := "claimed"
	if p.Verified {
		auth = "verified"
	}
	family, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	_ = s.IdentityAudit(r.Context(), identity.Observation{
		At: time.Now().UTC(), PrincipalID: p.ID, SessionID: p.SessionID,
		Mode: s.IdentityMode, Authentication: auth,
		Method: r.Method, Route: "/" + family, Event: eventTaskIDDropped,
	})
}
