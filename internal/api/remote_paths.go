package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/session"
)

// References describe resources within the selected environment. They neither
// encode host paths nor confer authority, and are never filesystem inputs.
func sessionPathRef(kind, id string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + id))
	return "tether-ref:" + kind + ":" + hex.EncodeToString(sum[:])
}

func sessionResponseFor(r *http.Request, dto SessionDTO) SessionDTO {
	if identity.IsRemote(r.Context()) && dto.Workspace != "" {
		dto.Workspace = sessionPathRef("workspace", dto.ID)
	}
	return dto
}

func launchResponseFor(r *http.Request, res LaunchResult) LaunchResponse {
	dto := launchResponseOf(res)
	if identity.IsRemote(r.Context()) {
		if dto.Workspace != "" {
			dto.Workspace = sessionPathRef("workspace", dto.ID)
		}
		if dto.Log != "" {
			dto.Log = sessionPathRef("log", dto.ID)
		}
	}
	return dto
}

type remoteOperationError struct {
	cause   error
	message string
}

func (e remoteOperationError) Error() string { return e.message }
func (e remoteOperationError) Unwrap() error { return e.cause }

// Only the response message is projected. Unwrap preserves existing Is/As
// classification, including native refusal codes and idempotency conflicts.
func remoteResponseError(r *http.Request, err error, category string) error {
	if identity.IsRemote(r.Context()) {
		return remoteOperationError{cause: err, message: category}
	}
	return err
}

func remoteResumeConflictError(r *http.Request, err error, nativeOnly bool) error {
	category := "session resume conflict"
	if nativeOnly && errors.Is(err, session.ErrRecoveryConflict) {
		// State the public requirements, not a specific inferred failed
		// predicate or any underlying error/path from the recorded context.
		category = "native-only resume unavailable; independent current launch authority required and recorded context must be valid"
	}
	return remoteResponseError(r, err, category)
}
