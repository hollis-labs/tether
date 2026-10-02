package sessionrefs

// extract.go (application policy) — turning the proxy from a call counter into a correlation source.
//
// S3 of SP-20260912-0001 (CW-20260912-0061); design record CW-20260912-0023.
// Typed reference extraction: CW-20260914-0003.
//
// The proxy already sits where every agent reaches Torque, Tesseract and
// Cerberus, and already holds the session identity.
//
// CW-20260914-0003 replaces Crockford ULID shape guessing with declared-field
// extraction and resolver binding:
//   1. Tesseract responses explicitly name their own kind (item_id, revision_id,
//      or ref.kind + ref.ref_id). Responses that declare their identity are
//      authoritative; we do not guess by scanning for ULID shapes.
//   2. Operation result status controls relation inference: workspace_write can
//      create, update, or replay. A replay produces NO new creation claim.
//   3. Search previews (tesseract_recall) and metadata-only resolution
//      (tesseract_ref_resolve) never count as content use (relationRead) and
//      do not extend lifetime.
//   4. Key-only captures (namespace + key with no typed ID in the response)
//      are resolved via tesseract_ref_resolve at capture time and bound to the
//      returned typed ref. Resolver failure preserves the unresolved capture
//      with bounded diagnostics and never fails the content operation.
//   5. Fallback argument scanning covers non-Tesseract tools (Torque tasks,
//      messaging URNs) within strict scan depth and value ceilings.
//
// THE PRIVACY LINE, AND WHY IT IS DRAWN HERE. The fingerprint was chosen
// deliberately: the portfolio has a standing position against caching payloads
// that may carry secrets, stated outright in ADR 0041 D18 (registry_entries has
// no cached_payload_json because substrate catalog YAMLs carry plaintext OAuth
// tokens). This opens that door exactly as far as declared identifier fields and
// an allowlist of identifier SHAPES and no further. Values that do not match a
// pattern or declared field are not stored, not logged, not counted.
//
// DIRECTION MATTERS. This READS outbound arguments into Tether's own store. It
// never adds anything to the forwarded call — that is CW-20260912-0024
// (proxy-stamped provenance), the opposite direction through the same seam, and
// it needs agreement from the receiving app. The two read as one idea and are
// not. proxy_boundary_test.go asserts the forwarded request is unchanged.

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

// RefKind mirrors store.SessionRefRow.Kind without importing the store: this
// package runs inside `tether mcp`, a separate process that reaches the daemon
// over HTTP and never opens the DB.
const (
	KindTorqueTask        = "torque_task"
	KindTesseractItem     = "tesseract_item"
	KindTesseractRevision = "tesseract_revision"
	KindTesseractKey      = "tesseract_key"
	KindMessagingURN      = "messaging_urn"
)

// Relations, matching store's vocabulary.
const (
	RelationCreated    = "created"
	RelationUpdated    = "updated"
	RelationRead       = "read"
	RelationReferenced = "referenced"
)

// Ref is one identifier observed in an outbound call.
type Ref struct {
	Kind         string
	RefID        string
	URI          string
	Relation     string
	ParentItemID string
}

// ResolvedRef is the result of resolving an identity via tesseract_ref_resolve.
type ResolvedRef struct {
	Status string `json:"status"`
	Ref    struct {
		Kind  string `json:"kind"`
		RefID string `json:"ref_id"`
		URI   string `json:"uri"`
	} `json:"ref"`
	ItemID     string `json:"item_id"`
	Domain     string `json:"domain"`
	ResolvedAt string `json:"resolved_at"`
}

// RefResolver is the seam for resolving key-only captures to canonical typed refs.
type RefResolver interface {
	ResolveRef(ctx context.Context, selector map[string]any) (*ResolvedRef, error)
}

// identifierPattern is one entry in the allowlist for fallback argument scanning.
type identifierPattern struct {
	kind string
	re   *regexp.Regexp
}

var identifierPatterns = []identifierPattern{
	// Torque task: CW-YYYYMMDD-NNNN.
	{KindTorqueTask, regexp.MustCompile(`^CW-\d{8}-\d{4}$`)},
	// Messaging URN.
	{KindMessagingURN, regexp.MustCompile(`^msg://[a-z]+/[^/]+/[^/]+$`)},
}

// Scan limits for argument scanning.
const (
	MaxScanDepth  = 8
	MaxScanValues = 512
)

type ScanResult struct {
	Refs    []Ref
	Refused bool
}

func strVal(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// Extract extracts identifiers from a completed tool call (request + response).
func (s *Service) Extract(ctx context.Context, toolName string, args map[string]any, resMap map[string]any) ScanResult {
	lowerName := strings.ToLower(toolName)

	// Gating: recall and resolver previews never count as content use.
	if lowerName == "tesseract_recall" || lowerName == "tesseract_ref_resolve" {
		return ScanResult{}
	}

	status := strVal(resMap, "status")

	// workspace_write replay: must produce NO new creation claim.
	if lowerName == "workspace_write" && status == "replayed" {
		return ScanResult{}
	}

	// 1. Check for declared typed response fields
	if resMap != nil {
		itemID := strVal(resMap, "item_id")
		if itemID == "" {
			if itemObj, ok := resMap["item"].(map[string]any); ok {
				itemID = strVal(itemObj, "item_id")
			}
		}
		if itemID == "" {
			if dataObj, ok := resMap["data"].(map[string]any); ok {
				itemID = strVal(dataObj, "item_id")
			}
		}

		revID := strVal(resMap, "revision_id")
		if revID == "" {
			if revObj, ok := resMap["revision"].(map[string]any); ok {
				revID = strVal(revObj, "revision_id")
				if itemID == "" {
					itemID = strVal(revObj, "item_id")
					if itemID == "" {
						itemID = strVal(revObj, "memory_id")
					}
				}
			}
		}
		if revID == "" {
			if dataObj, ok := resMap["data"].(map[string]any); ok {
				revID = strVal(dataObj, "revision_id")
				if itemID == "" {
					itemID = strVal(dataObj, "item_id")
					if itemID == "" {
						itemID = strVal(dataObj, "memory_id")
					}
				}
			}
		}
		if itemID == "" {
			itemID = strVal(resMap, "memory_id")
		}

		// Declared ref object in response (e.g. ref: {kind, ref_id, uri})
		if refObj, ok := resMap["ref"].(map[string]any); ok {
			refKind := strVal(refObj, "kind")
			refID := strVal(refObj, "ref_id")
			uri := strVal(refObj, "uri")
			if refKind != "" && refID != "" {
				rel := RelationForTool(toolName)
				switch status {
				case "created":
					rel = RelationCreated
				case "updated", "deleted":
					rel = RelationUpdated
				}
				return ScanResult{Refs: []Ref{{
					Kind:         refKind,
					RefID:        refID,
					URI:          uri,
					Relation:     rel,
					ParentItemID: itemID,
				}}}
			}
		}

		// Exact revision response (primary binding: tesseract_revision; parent item available as derived evidence)
		if revID != "" {
			rel := RelationForTool(toolName)
			if hasAnySuffix(lowerName, "_write", "_create") || status == "created" {
				rel = RelationCreated
			} else if hasAnySuffix(lowerName, "_update", "_edit") {
				rel = RelationUpdated
			} else if hasAnySuffix(lowerName, "_get", "_read") {
				rel = RelationRead
			}
			return ScanResult{Refs: []Ref{{
				Kind:         KindTesseractRevision,
				RefID:        revID,
				URI:          "tesseract://revision/" + revID,
				Relation:     rel,
				ParentItemID: itemID,
			}}}
		}

		// Exact item response (no revision_id)
		if itemID != "" {
			rel := RelationForTool(toolName)
			if status == "created" {
				rel = RelationCreated
			} else if status == "updated" || status == "deleted" || lowerName == "workspace_delete" {
				rel = RelationUpdated
			} else if hasAnySuffix(lowerName, "_get", "_read") {
				rel = RelationRead
			}
			return ScanResult{Refs: []Ref{{
				Kind:     KindTesseractItem,
				RefID:    itemID,
				URI:      "tesseract://item/" + itemID,
				Relation: rel,
			}}}
		}
	}

	// 2. Key-only capture: request specified namespace + key with no typed ID in response
	ns := strVal(args, "namespace")
	key := strVal(args, "key")
	if key == "" {
		key = strVal(args, "memory_key")
	}
	if ns != "" && key != "" {
		rel := RelationForTool(toolName)
		domain := strVal(args, "domain")
		if domain == "" {
			switch {
			case strings.HasPrefix(lowerName, "memory_"):
				domain = "memory"
			case strings.HasPrefix(lowerName, "knowledge_"):
				domain = "knowledge"
			case strings.HasPrefix(lowerName, "workspace_"):
				domain = "workspace"
			case strings.HasPrefix(lowerName, "event_"):
				domain = "event"
			default:
				domain = "knowledge"
			}
		}

		if s != nil && s.resolver != nil {
			resolved, err := s.resolver.ResolveRef(ctx, map[string]any{
				"domain":    domain,
				"namespace": ns,
				"key":       key,
			})
			if err == nil && resolved != nil && resolved.Status == "resolved" && resolved.Ref.RefID != "" {
				return ScanResult{Refs: []Ref{{
					Kind:         resolved.Ref.Kind,
					RefID:        resolved.Ref.RefID,
					URI:          resolved.Ref.URI,
					Relation:     rel,
					ParentItemID: resolved.ItemID,
				}}}
			}
			s.logger.Warn("session ref extraction: resolver failed for key capture; preserving unresolved capture",
				"tool", toolName, "namespace", ns, "key", key, "error", err)
		}

		return ScanResult{Refs: []Ref{{
			Kind:     KindTesseractKey,
			RefID:    ns + ":" + key,
			Relation: rel,
		}}}
	}

	// 3. Fallback argument scanning for non-Tesseract tools
	return ExtractArguments(toolName, args)
}

// ExtractArguments walks args and returns the identifiers matching the allowlist.
func ExtractArguments(toolName string, args map[string]any) ScanResult {
	rel := RelationForTool(toolName)
	seen := map[string]bool{}
	var out []Ref
	budget := MaxScanValues

	var walk func(v any, depth int) bool
	walk = func(v any, depth int) bool {
		if depth > MaxScanDepth {
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
					out = append(out, Ref{Kind: p.kind, RefID: t, Relation: rel})
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
			return ScanResult{Refused: true}
		}
	}
	return ScanResult{Refs: out}
}

// RelationForTool infers what the call did to the objects it names, from the
// tool-name verb. Unrecognized verbs fall back to "referenced" — the weakest
// true statement, rather than a guess that would overstate.
func RelationForTool(toolName string) string {
	name := strings.ToLower(toolName)
	switch {
	case hasAnySuffix(name, "_create", "_write", "_add", "_send", "_register", "_post"):
		return RelationCreated
	case hasAnySuffix(name, "_update", "_transition", "_edit", "_set", "_delete", "_archive", "_merge"):
		return RelationUpdated
	case hasAnySuffix(name, "_get", "_list", "_read", "_search", "_recall", "_show", "_history"):
		return RelationRead
	default:
		return RelationReferenced
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

// Attacher is the seam for writing extracted refs. The proxy runs in a
// separate process from the daemon and cannot touch the store, so this is an
// HTTP call in production (*client.Client) and a recorder in tests.
type Attacher interface {
	AttachSessionRef(ctx context.Context, sessionID, kind, refID, uri, relation, source, parentItemID string) error
}

// Observation contains the decoded SDK result without depending on MCP types.
type Observation struct {
	ToolName  string
	Arguments map[string]any
	Result    map[string]any
	Failed    bool
}

type Service struct {
	enabled   bool
	sessionID string
	resolver  RefResolver
	refs      Attacher
	logger    *slog.Logger
}

func New(enabled bool, sessionID string, resolver RefResolver, refs Attacher, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{enabled: enabled, sessionID: sessionID, resolver: resolver, refs: refs, logger: logger}
}

// Enabled preserves opt-in capture and requires both session identity and sink.
func (s *Service) Enabled() bool { return s.enabled && s.sessionID != "" && s.refs != nil }

// Record observes successful calls only. Refusal and attachment errors are
// logged and dropped; correlation never changes the operation it describes.
func (s *Service) Record(ctx context.Context, call Observation) {
	if !s.Enabled() || call.Failed {
		return
	}
	extracted := s.Extract(ctx, call.ToolName, call.Arguments, call.Result)
	if extracted.Refused {
		s.logger.Warn("session ref extraction skipped: argument scan exceeded its limit", "tool", call.ToolName, "max_values", MaxScanValues, "max_depth", MaxScanDepth)
		return
	}
	for _, ref := range extracted.Refs {
		if err := s.refs.AttachSessionRef(ctx, s.sessionID, ref.Kind, ref.RefID, ref.URI, ref.Relation, "proxy", ref.ParentItemID); err != nil {
			s.logger.Warn("attach session ref failed", "tool", call.ToolName, "kind", ref.Kind, "error", err)
		}
	}
}
