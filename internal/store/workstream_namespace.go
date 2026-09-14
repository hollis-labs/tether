package store

// workstream_namespace.go — where a workstream's contained content lives.
//
// S4 of SP-20260912-0001 (CW-20260912-0062); design record CW-20260912-0023.
//
// Tether owns the identity; Tesseract owns the content. Tether stores no note
// bodies, no scratch, no todo text — it resolves WHERE those live and records
// the returned item or revision id as a session ref. That is what keeps S2's "no
// content column" rule enforceable rather than aspirational: there is nowhere
// in Tether for the content to go, by construction.
//
// CONTAINMENT KEYS ON THE WORKSTREAM, NOT THE SESSION. A compaction creates a
// new session row, so scratch keyed on a session id is orphaned by the exact
// event the container exists to survive — the same failure S1 exists to
// prevent, one layer up.
//
// WORKSTREAM_ID IS AN ATTRIBUTE, NEVER A NAMESPACE PARTITION.
// Chrispian's ruling (2026-09-13, CW-20260912-0062):
// The earlier interim scheme placed notes under `session/ws_<id>/memory/notes`
// using the workstream ID as a pseudo-session. That is completely retired.
// Contained material belongs in Tesseract's declared workspace domain:
//   - Project-owned scratch: `project/<declared-project-id>/workspace/...`
//     (defaults to tail "scratch")
//   - Tether's own cross-project scratch: `app/tether/workspace/scratch`
// In both cases, `workstream_id` is an attribute attached to the content,
// NOT a path segment in the namespace.

import (
	"errors"
	"fmt"
	"strings"
)

// WorkstreamTargetOptions configures optional placement overrides for workspace scratch resolution.
type WorkstreamTargetOptions struct {
	// Project is the declared project identifier (e.g. "tether", "PRJ-20260418-0001").
	// When supplied, target is project/<project>/workspace/<tail>.
	Project string
	// Owner is an explicit scope head (e.g. "app/tether" or "project/foo").
	// When supplied, target is <owner>/workspace/<tail>.
	Owner string
	// Tail is the workspace segment, defaulting to "scratch".
	Tail string
}

// WorkstreamNamespace is where a workstream's contained content lives, and the
// workstream it belongs to.
type WorkstreamNamespace struct {
	// Namespace is the Tesseract workspace target (e.g. project/<id>/workspace/scratch or app/tether/workspace/scratch).
	Namespace string `json:"namespace"`
	// WorkstreamID is the workstream the content belongs to as an attribute.
	WorkstreamID string `json:"workstream_id"`
}

// ErrNotAWorkstream is returned when an id that should name a workstream does
// not. Distinct from ErrWorkstreamNotFound so the session-id case can say
// something more useful than "not found".
var ErrNotAWorkstream = errors.New("id does not name a workstream")

// SessionWorkstreamNamespace resolves a SESSION to the Tesseract workspace target
// its workstream's contained content belongs in, accompanied by its workstream ID
// as an attribute.
//
// It takes a session id rather than a workstream id: a session is what callers hold.
// If neither an explicit Project nor an Owner is passed in opts, the resolver reads
// the session's declared project_id. If the session has no declared project_id,
// the resolver refuses the ownerless session.
//
// It deliberately does NOT auto-create a workstream on read.
func (s *Store) SessionWorkstreamNamespace(sessionID string, opts ...WorkstreamTargetOptions) (WorkstreamNamespace, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return WorkstreamNamespace{}, errors.New("workstream namespace: session id required")
	}

	row, err := s.GetSession(sessionID)
	if err != nil {
		return WorkstreamNamespace{}, fmt.Errorf("workstream namespace for session %q: %w", sessionID, err)
	}
	if !row.WorkstreamID.Valid || row.WorkstreamID.String == "" {
		// Deliberately not auto-creating one. EnsureSessionWorkstream exists
		// and is one call away, but doing it implicitly here would mean a
		// namespace lookup silently mutates the session's lineage — a read
		// that writes. The caller asks for a container explicitly or is told
		// it has none.
		return WorkstreamNamespace{}, fmt.Errorf("session %q belongs to no workstream: call EnsureSessionWorkstream first (%w)", sessionID, ErrWorkstreamNotFound)
	}

	var opt WorkstreamTargetOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	// If no explicit project or owner is given, try session's declared project_id.
	if strings.TrimSpace(opt.Project) == "" && strings.TrimSpace(opt.Owner) == "" {
		if strings.TrimSpace(row.ProjectID) != "" {
			opt.Project = strings.TrimSpace(row.ProjectID)
		} else {
			return WorkstreamNamespace{}, errors.New("workstream namespace: session has no declared project: specify explicit project or owner")
		}
	}

	return s.WorkstreamTarget(row.WorkstreamID.String, opt)
}

// WorkstreamTarget resolves a WORKSTREAM ID directly to its workspace target,
// returning the target namespace and the workstream ID attribute.
//
// It verifies that workstreamID names a real workstream, refusing bare session IDs
// with ErrNotAWorkstream.
func (s *Store) WorkstreamTarget(workstreamID string, opts ...WorkstreamTargetOptions) (WorkstreamNamespace, error) {
	workstreamID = strings.TrimSpace(workstreamID)
	if workstreamID == "" {
		return WorkstreamNamespace{}, errors.New("workstream target: workstream id required")
	}

	if _, err := s.GetWorkstream(workstreamID); err != nil {
		if errors.Is(err, ErrWorkstreamNotFound) {
			// Catch a session id arriving where a workstream id is expected.
			if _, sErr := s.GetSession(workstreamID); sErr == nil {
				return WorkstreamNamespace{}, fmt.Errorf("%q is a session id, not a workstream id: containment keys on the workstream so it survives a compaction, which creates a new session row (%w)", workstreamID, ErrNotAWorkstream)
			}
			return WorkstreamNamespace{}, fmt.Errorf("workstream %q: %w", workstreamID, ErrNotAWorkstream)
		}
		return WorkstreamNamespace{}, err
	}

	var opt WorkstreamTargetOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	tail := strings.TrimSpace(opt.Tail)
	if tail == "" {
		tail = "scratch"
	}
	tail = strings.Trim(tail, "/")
	if tail == "" {
		return WorkstreamNamespace{}, errors.New("workstream target: workspace requires a non-empty tail segment")
	}

	var scopeHead string
	if strings.TrimSpace(opt.Owner) != "" {
		owner := strings.TrimSpace(opt.Owner)
		owner = strings.Trim(owner, "/")
		scopeHead = owner
	} else if strings.TrimSpace(opt.Project) != "" {
		proj := strings.TrimSpace(opt.Project)
		proj = strings.TrimPrefix(proj, "project/")
		proj = strings.Trim(proj, "/")
		scopeHead = "project/" + proj
	} else {
		return WorkstreamNamespace{}, errors.New("workstream target: declared project or owner is required")
	}

	target := scopeHead + "/workspace/" + tail
	return WorkstreamNamespace{
		Namespace:    target,
		WorkstreamID: workstreamID,
	}, nil
}
