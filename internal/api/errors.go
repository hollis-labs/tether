// Package api owns the HTTP surface of the tether daemon: DTOs, handlers,
// and router composition. The daemon package wraps this with listener +
// PID-file lifecycle; clients import this package for the DTO types.
package api

import (
	"encoding/json"
	"net/http"
)

// Standard error codes returned in ErrorResponse.Error.Code. External
// clients key on these; add sparingly.
const (
	CodeInvalidRequest   = "invalid_request"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodePayloadTooLarge  = "payload_too_large"
	CodeConflict         = "conflict"
	CodeInternalError    = "internal_error"
	CodeNotImplemented   = "not_implemented"

	// CodeProviderSessionLost (409) means the provider no longer has the
	// session's resume id: the turn was not delivered, the runtime has
	// dropped the id, and resending the same request starts a fresh provider
	// session without the old history. Split from CodeConflict so callers
	// can tell it from a session that cannot take input at all.
	CodeProviderSessionLost = "provider_session_lost"

	// CodeTurnFailed (502) means the session's agent process ran the turn
	// and exited non-zero — a subprocess runtime (codex exec, claude -p)
	// that failed on its own terms, such as a provider 401 or a usage
	// error. The daemon is fine and the session stays up; the message
	// carries the exit status and a bounded tail of the process's stderr
	// (CW-20261001-0033).
	CodeTurnFailed = "turn_failed"

	// CodeIdempotencyConflict (409) means an idempotency key was reused with
	// a different request. The key stays bound to its original session
	// (CW-20260930-0229).
	CodeIdempotencyConflict = "idempotency_conflict"
)

// ErrorResponse is the envelope for every non-2xx JSON body. Callers
// read the nested code programmatically; the message is human-oriented.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the nested object inside ErrorResponse.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Error: ErrorDetail{Code: code, Message: message}})
}
