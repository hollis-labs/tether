// Package api owns the HTTP surface of the mux daemon: DTOs, handlers,
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
