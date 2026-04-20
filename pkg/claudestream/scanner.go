package claudestream

import (
	"bufio"
	"io"
)

// Scanner reads newline-delimited claude stream-json events from an
// io.Reader and yields parsed Events one at a time.
//
// Usage:
//
//	sc := claudestream.NewScanner(r)
//	for {
//	    ev, ok, err := sc.Next()
//	    if err != nil { ... }
//	    if !ok { break }
//	    // ... handle ev
//	}
//
// Scanner buffers one line at a time; a single line may produce
// multiple Events (an assistant message with text + tool_use produces
// two). The scanner stores pending events internally and drains them
// across Next calls.
type Scanner struct {
	r       *bufio.Scanner
	pending []Event
}

// NewScanner wraps r in a line-buffered scanner. The default
// bufio.Scanner buffer (64 KiB) is sufficient for every claude
// event observed; for unusually large messages, callers can set a
// larger buffer via Scanner.SetBuffer before calling Next.
func NewScanner(r io.Reader) *Scanner {
	return &Scanner{r: bufio.NewScanner(r)}
}

// SetBuffer overrides the underlying bufio.Scanner's buffer. Call
// before the first Next. Mirrors bufio.Scanner.Buffer semantics.
func (s *Scanner) SetBuffer(buf []byte, maxBytes int) {
	s.r.Buffer(buf, maxBytes)
}

// Next yields the next Event. Returns (event, true, nil) on success,
// (zero-Event, false, nil) at clean EOF, or (zero-Event, false, err)
// on a parse error. Parse errors are returned once per bad line —
// subsequent Next calls continue reading.
//
// Callers loop until ok==false or err!=nil.
func (s *Scanner) Next() (Event, bool, error) {
	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]
		return ev, true, nil
	}
	for s.r.Scan() {
		line := s.r.Bytes()
		events, err := Parse(line)
		if err != nil {
			return Event{}, false, err
		}
		if len(events) == 0 {
			continue
		}
		s.pending = events[1:]
		return events[0], true, nil
	}
	if err := s.r.Err(); err != nil {
		return Event{}, false, err
	}
	return Event{}, false, nil
}
