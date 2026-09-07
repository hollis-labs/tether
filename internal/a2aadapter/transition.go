package a2aadapter

// transition.go — the ONLY way a delegated task's outcome is decided
// (see doc.go / executor.go). A typed, explicitly authorized HTTP call,
// matching T09's authorized_by convention (self-asserted URN identity,
// ADR 0045) -- not an implicit inference from a generic message.
//
// This endpoint is a bare http.HandlerFunc (adapter.go), NOT one of the
// RequestHandler methods a2asrv.NewHandler/WithCallInterceptors wraps --
// the SDK's CallInterceptor extension point has no relationship to a
// handler Tether mounts itself. An independent review of this task found
// that the first version of this file relied on that machinery anyway
// and therefore checked NO authentication at all here, regardless of a
// binding's configured BearerToken: the very peer that submitted a
// delegated task could read its own TaskID off the Submitted event and
// immediately self-resolve it via this endpoint, completely defeating
// the "a consumer decides, not the peer" guarantee doc.go describes.
// checkTransitionBearerToken below is this endpoint's own, independent
// enforcement of the same per-binding token -- and taskCoordinator.resolve
// additionally verifies the URL's bindingID actually owns taskID, closing
// a second gap the same review found: task IDs are process-wide unique
// UUIDs shared across one taskCoordinator for every binding (adapter.go),
// so without that check a caller could target a DIFFERENT binding's task
// through this one's URL.

import (
	"encoding/json"
	"net/http"
	"strings"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// transitionableStates is the closed set of states a consumer may move a
// delegated task to via this endpoint. Canceled is deliberately excluded
// -- the protocol's own CancelTask RPC (routed through TetherExecutor.Cancel)
// is the canonical way to cancel; duplicating that here would be a second,
// divergent cancellation path.
var transitionableStates = map[string]a2a.TaskState{
	"completed":      a2a.TaskStateCompleted,
	"failed":         a2a.TaskStateFailed,
	"input_required": a2a.TaskStateInputRequired,
}

type transitionRequest struct {
	// AuthorizedBy is a URN-shaped caller identity, self-asserted per
	// ADR 0045 -- matching every other authorized-write action on this
	// codebase's messaging surface (T09's redrive/purge).
	AuthorizedBy string `json:"authorized_by"`
	// State must be one of transitionableStates' keys.
	State string `json:"state"`
	// ResultText, when non-empty, becomes the text of the a2a.Message
	// attached to the terminal status update the blocked A2A caller
	// receives.
	ResultText string `json:"result_text,omitempty"`
}

type transitionResponse struct {
	TaskID string `json:"task_id"`
	// Resolved is false when no Execute call was waiting for this task
	// id -- already resolved, already timed out, or an unknown id. A
	// caller seeing Resolved=false got an honest non-success, never a
	// silent no-op mistaken for a real transition (T10 acceptance #3
	// mirrors T09's "no silent deletion" discipline here: no silent
	// no-op either).
	Resolved bool `json:"resolved"`
}

func writeTransitionError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleTransition services POST /agents/{bindingID}/tasks/{taskID}/transition.
// bearerToken is that binding's own configured token (empty means no
// check, matching bearerTokenInterceptor's convention for the JSON-RPC
// path) -- checked here directly since, per this file's doc comment,
// nothing else on this endpoint's path does.
func (a *Adapter) handleTransition(w http.ResponseWriter, r *http.Request, bindingID, taskID, bearerToken string) {
	if r.Method != http.MethodPost {
		writeTransitionError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !matchesBearerToken(r.Header.Get("Authorization"), bearerToken) {
		writeTransitionError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}
	var req transitionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeTransitionError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.AuthorizedBy == "" {
		writeTransitionError(w, http.StatusBadRequest, "authorized_by is required")
		return
	}
	if _, err := messaging.ParseURN(req.AuthorizedBy); err != nil {
		writeTransitionError(w, http.StatusBadRequest, "authorized_by must be a valid URN: "+err.Error())
		return
	}
	state, ok := transitionableStates[strings.ToLower(req.State)]
	if !ok {
		writeTransitionError(w, http.StatusBadRequest, "state must be one of: completed, failed, input_required")
		return
	}

	var resultMsg *a2a.Message
	if req.ResultText != "" {
		resultMsg = a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(req.ResultText))
	}

	resolved, wrongBinding := a.coordinator.resolve(a2a.TaskID(taskID), bindingID, taskOutcome{state: state, message: resultMsg})
	if wrongBinding {
		writeTransitionError(w, http.StatusNotFound, "no such task on this binding")
		return
	}
	status := http.StatusOK
	if !resolved {
		status = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(transitionResponse{TaskID: taskID, Resolved: resolved})
}
