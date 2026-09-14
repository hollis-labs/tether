package mcpadapter

// extract.go — turning the proxy from a call counter into a correlation source.
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
	"encoding/json"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// RefKind mirrors store.SessionRefRow.Kind without importing the store: this
// package runs inside `mux mcp`, a separate process that reaches the daemon
// over HTTP and never opens the DB.
const (
	refKindTorqueTask        = "torque_task"
	refKindTesseractItem     = "tesseract_item"
	refKindTesseractRevision = "tesseract_revision"
	refKindTesseractKey      = "tesseract_key"
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
	{refKindTorqueTask, regexp.MustCompile(`^CW-\d{8}-\d{4}$`)},
	// Messaging URN.
	{refKindMessagingURN, regexp.MustCompile(`^msg://[a-z]+/[^/]+/[^/]+$`)},
}

// Scan limits for argument scanning.
const (
	maxScanDepth  = 8
	maxScanValues = 512
)

type scanResult struct {
	refs    []extractedRef
	refused bool
}

// parseResultMap extracts the top-level structured JSON map from a CallToolResult.
func parseResultMap(res *mcp.CallToolResult) map[string]any {
	if res == nil {
		return nil
	}
	if m, ok := res.StructuredContent.(map[string]any); ok && len(m) > 0 {
		return m
	}
	for _, c := range res.Content {
		if tc, ok := mcp.AsTextContent(c); ok && tc != nil {
			var m map[string]any
			if err := json.Unmarshal([]byte(tc.Text), &m); err == nil && len(m) > 0 {
				return m
			}
		}
	}
	return nil
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

// extractCallRefs extracts identifiers from a completed tool call (request + response).
func (a *Adapter) extractCallRefs(ctx context.Context, toolName string, args map[string]any, res *mcp.CallToolResult) scanResult {
	lowerName := strings.ToLower(toolName)

	// Gating: recall and resolver previews never count as content use.
	if lowerName == "tesseract_recall" || lowerName == "tesseract_ref_resolve" {
		return scanResult{}
	}

	resMap := parseResultMap(res)
	status := strVal(resMap, "status")

	// workspace_write replay: must produce NO new creation claim.
	if lowerName == "workspace_write" && status == "replayed" {
		return scanResult{}
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
				rel := relationForTool(toolName)
				switch status {
				case "created":
					rel = relationCreated
				case "updated", "deleted":
					rel = relationUpdated
				}
				return scanResult{refs: []extractedRef{{
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
			rel := relationForTool(toolName)
			if hasAnySuffix(lowerName, "_write", "_create") || status == "created" {
				rel = relationCreated
			} else if hasAnySuffix(lowerName, "_update", "_edit") {
				rel = relationUpdated
			} else if hasAnySuffix(lowerName, "_get", "_read") {
				rel = relationRead
			}
			return scanResult{refs: []extractedRef{{
				Kind:         refKindTesseractRevision,
				RefID:        revID,
				URI:          "tesseract://revision/" + revID,
				Relation:     rel,
				ParentItemID: itemID,
			}}}
		}

		// Exact item response (no revision_id)
		if itemID != "" {
			rel := relationForTool(toolName)
			if status == "created" {
				rel = relationCreated
			} else if status == "updated" || status == "deleted" || lowerName == "workspace_delete" {
				rel = relationUpdated
			} else if hasAnySuffix(lowerName, "_get", "_read") {
				rel = relationRead
			}
			return scanResult{refs: []extractedRef{{
				Kind:     refKindTesseractItem,
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
		rel := relationForTool(toolName)
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

		if a != nil && a.resolver != nil {
			resolved, err := a.resolver.ResolveRef(ctx, map[string]any{
				"domain":    domain,
				"namespace": ns,
				"key":       key,
			})
			if err == nil && resolved != nil && resolved.Status == "resolved" && resolved.Ref.RefID != "" {
				return scanResult{refs: []extractedRef{{
					Kind:         resolved.Ref.Kind,
					RefID:        resolved.Ref.RefID,
					URI:          resolved.Ref.URI,
					Relation:     rel,
					ParentItemID: resolved.ItemID,
				}}}
			}
			a.logger().Warn("session ref extraction: resolver failed for key capture; preserving unresolved capture",
				"tool", toolName, "namespace", ns, "key", key, "error", err)
		}

		return scanResult{refs: []extractedRef{{
			Kind:     refKindTesseractKey,
			RefID:    ns + ":" + key,
			Relation: rel,
		}}}
	}

	// 3. Fallback argument scanning for non-Tesseract tools
	return extractRefsFromArgs(toolName, args)
}

// extractRefsFromArgs walks args and returns the identifiers matching the allowlist.
func extractRefsFromArgs(toolName string, args map[string]any) scanResult {
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

// extractRefs is preserved for argument-only extraction tests.
func extractRefs(toolName string, args map[string]any) scanResult {
	return extractRefsFromArgs(toolName, args)
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
	AttachSessionRef(ctx context.Context, sessionID, kind, refID, uri, relation, source, parentItemID string) error
}

// extractionEnabled reports whether extraction should run.
//
// OFF BY DEFAULT, per the task: this captures argument/response
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
func (a *Adapter) recordRefs(ctx context.Context, req mcp.CallToolRequest, res *mcp.CallToolResult, callErr error) {
	if !a.extractionEnabled() || callErr != nil || (res != nil && res.IsError) {
		return
	}
	extracted := a.extractCallRefs(ctx, req.Params.Name, req.GetArguments(), res)
	if extracted.refused {
		a.logger().Warn("session ref extraction skipped: argument scan exceeded its limit",
			"tool", req.Params.Name, "max_values", maxScanValues, "max_depth", maxScanDepth)
		return
	}
	for _, ref := range extracted.refs {
		if err := a.refs.AttachSessionRef(ctx, a.SessionID, ref.Kind, ref.RefID, ref.URI, ref.Relation, "proxy", ref.ParentItemID); err != nil {
			a.logger().Warn("attach session ref failed",
				"tool", req.Params.Name, "kind", ref.Kind, "error", err)
		}
	}
}
