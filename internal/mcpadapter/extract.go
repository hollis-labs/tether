package mcpadapter

// extract.go — turning the proxy from a call counter into a correlation source.
//
// S3 of SP-20260912-0001 (CW-20260912-0061); design record CW-20260912-0023.
//
// The proxy already sits where every agent reaches Torque, Tesseract and
// Cerberus, and already holds the session identity. What it recorded was a
// FINGERPRINT OF THE ARGUMENT SHAPE and no values:
//
//	mux  torque_task_get   args_schema_fp=a5614527  ok=1
//
// So Tether could say a session called torque_task_get four times and not
// which task. This extracts the identifiers and nothing else.
//
// THE PRIVACY LINE, AND WHY IT IS DRAWN HERE. The fingerprint was chosen
// deliberately: the portfolio has a standing position against caching payloads
// that may carry secrets, stated outright in ADR 0041 D18 (registry_entries has
// no cached_payload_json because substrate catalog YAMLs carry plaintext OAuth
// tokens). This opens that door exactly as far as an allowlist of identifier
// SHAPES and no further. Values that do not match a pattern are not stored, not
// logged, not counted. If a future change finds itself persisting an args blob
// "for now", that is the wrong shape and belongs back in discussion.
//
// DIRECTION MATTERS. This READS outbound arguments into Tether's own store. It
// never adds anything to the forwarded call — that is CW-20260912-0024
// (proxy-stamped provenance), the opposite direction through the same seam, and
// it needs agreement from the receiving app. The two read as one idea and are
// not. proxy_boundary_test.go asserts the forwarded request is unchanged.

import (
	"context"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// RefKind mirrors store.SessionRefRow.Kind without importing the store: this
// package runs inside `mux mcp`, a separate process that reaches the daemon
// over HTTP and never opens the DB.
const (
	refKindTorqueTask        = "torque_task"
	refKindTesseractRevision = "tesseract_revision"
	refKindMessagingURN      = "messaging_urn"
)

// Relations, matching store's vocabulary.
const (
	relationCreated    = "created"
	relationUpdated    = "updated"
	relationRead       = "read"
	relationReferenced = "referenced"
)

// extractedRef is one identifier observed in an outbound call.
type extractedRef struct {
	Kind     string
	RefID    string
	Relation string
}

// identifierPattern is one entry in the allowlist.
//
// Patterns are Go-defined rather than configuration, and that is the simpler
// option as well as the safer one. Config-supplied regexes would run on a path
// every proxied call traverses, so one pathological pattern stalls every
// session — a cost with no offsetting benefit, since adding an identifier shape
// is rare and is correctly a code change. "Configurable" in the task means "not
// a per-tool argument map", which the pattern approach already delivers: it
// covers tools Tether does not own and degrades to capturing NOTHING rather
// than capturing the wrong thing.
type identifierPattern struct {
	kind string
	re   *regexp.Regexp
}

var identifierPatterns = []identifierPattern{
	// Torque task: CW-YYYYMMDD-NNNN.
	{refKindTorqueTask, regexp.MustCompile(`^CW-\d{8}-\d{4}$`)},
	// Tesseract revision / memory id: 26-character Crockford base32 ULID.
	// Crockford excludes I, L, O and U to avoid transcription ambiguity.
	{refKindTesseractRevision, regexp.MustCompile(`^[0-9ABCDEFGHJKMNPQRSTVWXYZ]{26}$`)},
	// Messaging URN.
	{refKindMessagingURN, regexp.MustCompile(`^msg://[a-z]+/[^/]+/[^/]+$`)},
}

// Scan limits. Depth alone is not enough: a WIDE payload — hundreds of string
// values times N patterns — costs as much as a deep one, on a path every call
// traverses.
//
// On hitting either limit the scan yields NOTHING rather than a partial set. A
// partial extraction is a silently incomplete ref set, which is the failure
// mode hardest to notice later: the digest looks answered rather than
// truncated. Refusing is legible; half an answer is not.
const (
	maxScanDepth  = 8
	maxScanValues = 512
)

// scanResult carries the refs, or the fact that the scan was abandoned.
//
// The field is `refused`, not `truncated`, because the behavior is refusal:
// nothing is kept. A field named truncated would describe the opposite of what
// happens, and the tempting way to reconcile a name with its code is to change
// the code — which here would mean silently producing partial ref sets, the
// exact failure the refusal exists to prevent.
type scanResult struct {
	refs    []extractedRef
	refused bool
}

// extractRefs walks args and returns the identifiers matching the allowlist.
//
// relation is derived from the tool-name verb, so one rule covers every tool
// including ones Tether did not write.
func extractRefs(toolName string, args map[string]any) scanResult {
	rel := relationForTool(toolName)
	seen := map[string]bool{}
	var out []extractedRef
	budget := maxScanValues

	var walk func(v any, depth int) bool
	walk = func(v any, depth int) bool {
		if depth > maxScanDepth {
			return false
		}
		switch t := v.(type) {
		case string:
			if budget <= 0 {
				return false
			}
			budget--
			for _, p := range identifierPatterns {
				if !p.re.MatchString(t) {
					continue
				}
				key := p.kind + "\x00" + t
				if !seen[key] {
					seen[key] = true
					out = append(out, extractedRef{Kind: p.kind, RefID: t, Relation: rel})
				}
				break
			}
		case map[string]any:
			for _, child := range t {
				if !walk(child, depth+1) {
					return false
				}
			}
		case []any:
			for _, child := range t {
				if !walk(child, depth+1) {
					return false
				}
			}
		}
		return true
	}

	for _, v := range args {
		if !walk(v, 0) {
			return scanResult{refused: true}
		}
	}
	return scanResult{refs: out}
}

// relationForTool infers what the call did to the objects it names, from the
// tool-name verb. Unrecognized verbs fall back to "referenced" — the weakest
// true statement, rather than a guess that would overstate.
func relationForTool(toolName string) string {
	name := strings.ToLower(toolName)
	switch {
	case hasAnySuffix(name, "_create", "_write", "_add", "_send", "_register", "_post"):
		return relationCreated
	case hasAnySuffix(name, "_update", "_transition", "_edit", "_set", "_delete", "_archive", "_merge"):
		return relationUpdated
	case hasAnySuffix(name, "_get", "_list", "_read", "_search", "_recall", "_show", "_history"):
		return relationRead
	default:
		return relationReferenced
	}
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

// refAttacher is the seam for writing extracted refs. The proxy runs in a
// separate process from the daemon and cannot touch the store, so this is an
// HTTP call in production (*client.Client) and a recorder in tests.
type refAttacher interface {
	AttachSessionRef(ctx context.Context, sessionID string, kind, refID, relation, source string) error
}

// extractionEnabled reports whether S3 extraction should run.
//
// OFF BY DEFAULT, per the task: this is the first thing to capture argument
// VALUES rather than shapes, and Chrispian wants to see what it captures on
// real traffic before it is on for everyone.
func (a *Adapter) extractionEnabled() bool {
	return a.ExtractRefs && a.SessionID != "" && a.refs != nil
}

// recordRefs extracts identifiers from an outbound call and attaches them to
// the session.
//
// ONLY ON SUCCESS. A torque_task_create that errored did not create the task,
// and session_refs has no column to say a call failed — the triple is closed.
// Recording relation=created for it would assert something that did not
// happen, so a failed call leaves no ref at all.
//
// Failures to attach are logged and dropped. Correlation is a side effect of a
// proxied call; it must never fail the call it describes.
func (a *Adapter) recordRefs(ctx context.Context, req mcp.CallToolRequest, callOK bool) {
	if !a.extractionEnabled() || !callOK {
		return
	}
	res := extractRefs(req.Params.Name, req.GetArguments())
	if res.refused {
		a.logger().Warn("session ref extraction skipped: argument scan exceeded its limit",
			"tool", req.Params.Name, "max_values", maxScanValues, "max_depth", maxScanDepth)
		return
	}
	for _, ref := range res.refs {
		if err := a.refs.AttachSessionRef(ctx, a.SessionID, ref.Kind, ref.RefID, ref.Relation, "proxy"); err != nil {
			a.logger().Warn("attach session ref failed",
				"tool", req.Params.Name, "kind", ref.Kind, "error", err)
		}
	}
}
