package mcpadapter

import (
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/redact"
)

// redactedError is an upstream error whose text has had the upstream's
// credential values scrubbed. It unwraps to the original, so errors.Is and
// errors.As still see the cause; only the text that gets logged, stored in a
// ServerStatus or returned to a tool caller is changed.
type redactedError struct {
	text string
	err  error
}

func (e *redactedError) Error() string { return e.text }
func (e *redactedError) Unwrap() error { return e.err }

// redactUpstreamError scrubs entry's credential values from err's text.
//
// A connect or list-tools error names the endpoint it failed on, so for an
// sse/http upstream whose url came from a file:// or keychain:// reference, or
// carries a ${VAR} token, the raw error would put the secret into the
// "upstream unavailable" log line and into ServerStatus.Error, which
// tether_health and the sysop API return. Errors with nothing to scrub are
// returned as they were.
//
// This is error text, which is whole, so it uses internal/redact (exact matches
// only, nothing shorter than four bytes), not supervise.Redact. supervise.Redact
// is written for a stderr tail that can be cut mid-value: it has no minimum
// length and blanks a secret's edge fragments, so a short env value such as "1"
// or "on" turned "connect" into "c[redacted]nect".
func redactUpstreamError(err error, entry config.MCPServerEntry) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	if got := redact.Text(text, stderrRedactionValues(entry)); got != text {
		return &redactedError{text: got, err: err}
	}
	return err
}
