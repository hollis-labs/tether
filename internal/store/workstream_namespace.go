package store

// workstream_namespace.go — where a workstream's contained content lives.
//
// S4 of SP-20260912-0001 (CW-20260912-0062); design record CW-20260912-0023.
//
// Tether owns the identity; Tesseract owns the content. Tether stores no note
// bodies, no scratch, no todo text — it resolves WHERE those live and records
// the returned revision id as a session ref. That is what keeps S2's "no
// content column" rule enforceable rather than aspirational: there is nowhere
// in Tether for the content to go, by construction.
//
// CONTAINMENT KEYS ON THE WORKSTREAM, NOT THE SESSION. A compaction creates a
// new session row, so scratch keyed on a session id is orphaned by the exact
// event the container exists to survive — the same failure S1 exists to
// prevent, one layer up.
//
// THE NAMESPACE, AND THE COMPROMISE IN IT. Tesseract's memory grammar is
// user/{id}/session/{sid}/memory/{type}. There is no workstream segment, so a
// workstream id goes in the {sid} slot, and the segment then says "session"
// about something that deliberately is not one.
//
// The id is therefore PREFIXED — ws_<workstream-id> — which is not cosmetic.
// A bare id in that slot fails SILENTLY: an agent correlates it against
// sessions.id, gets no rows, and no-rows is indistinguishable from a session
// that touched nothing. A ws_ prefix fails LOUDLY, because no session id
// starts with it, so a mis-correlation is visibly wrong at the point it is
// made rather than quietly empty.
//
// This is a compromise and is recorded as one: asking Tesseract for a real
// workstream segment is the honest end state (CW-20260912-0111), and the
// migration is stripping the prefix — which is the property that made the
// prefix acceptable rather than merely convenient.

import (
	"errors"
	"fmt"
	"strings"
)

// workstreamSIDPrefix marks a {sid} segment that holds a workstream id rather
// than a session id. Deliberately not exported: the whole point is that
// nothing outside this file assembles the string.
const workstreamSIDPrefix = "ws_"

// ErrNotAWorkstream is returned when an id that should name a workstream does
// not. Distinct from ErrWorkstreamNotFound so the session-id case can say
// something more useful than "not found".
var ErrNotAWorkstream = errors.New("id does not name a workstream")

// SessionWorkstreamNamespace resolves a SESSION to the Tesseract namespace its
// workstream's contained content belongs in.
//
// It takes a session id rather than a workstream id on purpose. A session is
// what a caller actually has — the workstream is an internal resolution — and
// an API that only accepts the thing callers hold cannot be handed the wrong
// one. Combined with the unexported prefix, that is what makes the namespace
// unconstructible by hand anywhere in Tether: there is no exported path that
// takes a workstream id, and no exported constant to concatenate.
//
// memoryType is passed through UNVALIDATED and that is deliberate. The type
// vocabulary (decisions, notes, todos, ...) is Tesseract's, and mirroring it
// here would create a second copy that drifts — Tether would start rejecting a
// type Tesseract had just added. An unknown type fails at write time, from the
// system that owns the vocabulary, which is where that error belongs.
//
// userID is a parameter because Tether does not own the Tesseract user
// identity and should not invent one.
func (s *Store) SessionWorkstreamNamespace(userID, sessionID, memoryType string) (string, error) {
	switch {
	case strings.TrimSpace(userID) == "":
		return "", errors.New("workstream namespace: user id required")
	case strings.TrimSpace(sessionID) == "":
		return "", errors.New("workstream namespace: session id required")
	case strings.TrimSpace(memoryType) == "":
		return "", errors.New("workstream namespace: memory type required")
	}

	row, err := s.GetSession(sessionID)
	if err != nil {
		return "", fmt.Errorf("workstream namespace for session %q: %w", sessionID, err)
	}
	if !row.WorkstreamID.Valid || row.WorkstreamID.String == "" {
		// Deliberately not auto-creating one. EnsureSessionWorkstream exists
		// and is one call away, but doing it implicitly here would mean a
		// namespace lookup silently mutates the session's lineage — a read
		// that writes. The caller asks for a container explicitly or is told
		// it has none.
		return "", fmt.Errorf("session %q belongs to no workstream: call EnsureSessionWorkstream first (%w)", sessionID, ErrWorkstreamNotFound)
	}
	return s.workstreamNamespace(userID, row.WorkstreamID.String, memoryType)
}

// workstreamNamespace builds the namespace for a workstream id.
//
// Unexported, and it still verifies the id names a real workstream rather than
// trusting its caller. That guard is not redundant with the only current
// caller having just read the id off a session row: it is the guard that
// catches a FUTURE second entry point taking a workstream id from outside, and
// specifically the case of a session id arriving where a workstream id is
// expected. Constructing ws_<session-id> would produce a perfectly well-formed
// namespace containing a lie, which is the one input where the prefix makes
// things worse rather than better.
func (s *Store) workstreamNamespace(userID, workstreamID, memoryType string) (string, error) {
	if _, err := s.GetWorkstream(workstreamID); err != nil {
		if errors.Is(err, ErrWorkstreamNotFound) {
			// Say which mistake it probably is. Existence is checked rather
			// than id shape because shape is a heuristic that rots: session
			// ids are uuidv4 and workstream ids uuidv7 today, and nothing
			// keeps that true.
			if _, sErr := s.GetSession(workstreamID); sErr == nil {
				return "", fmt.Errorf("%q is a session id, not a workstream id: containment keys on the workstream so it survives a compaction, which creates a new session row (%w)", workstreamID, ErrNotAWorkstream)
			}
			return "", fmt.Errorf("workstream %q: %w", workstreamID, ErrNotAWorkstream)
		}
		return "", err
	}
	return fmt.Sprintf("user/%s/session/%s%s/memory/%s",
		userID, workstreamSIDPrefix, workstreamID, memoryType), nil
}
