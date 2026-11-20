package mcpadapter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/telemetry"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yosida95/uritemplate/v3"
)

type resourceOrigin struct {
	id     string
	client resourceClient
}
type resourceRecord struct {
	origin   resourceOrigin
	resource *mcpsdk.Resource
	template *mcpsdk.ResourceTemplate
}

func (p *ClientPool) resourceOrigins(allowed map[string]bool) []resourceOrigin {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []resourceOrigin{}
	for id, status := range p.statuses {
		if allowed != nil && !allowed[id] || status.state != "connected" {
			continue
		}
		client, ok := status.client.(resourceClient)
		if !ok {
			continue
		}
		initialized := client.InitializeResult()
		if initialized == nil || initialized.Capabilities == nil || initialized.Capabilities.Resources == nil {
			continue
		}
		out = append(out, resourceOrigin{id, client})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func resourceUnavailable() error {
	return &jsonrpc.Error{Code: -32002, Message: "resource unavailable under this gateway view"}
}
func resourceInventoryError() error {
	return &jsonrpc.Error{Code: -32603, Message: "upstream resource inventory unavailable"}
}

// Lists are recomputed against the live connections on every request, so a
// reconnect or a tightened caller view cannot retain stale dispatch authority.
// The catalog is bounded and repeated upstream cursors fail instead of looping.
func resourceCatalog(ctx context.Context, origins []resourceOrigin, meta mcpsdk.Meta, templates bool, next mcpsdk.MethodHandler, req mcpsdk.Request) ([]resourceRecord, mcpsdk.Meta, error) {
	records := []resourceRecord{}
	upstreamMeta := map[string]any{}
	for _, origin := range append([]resourceOrigin{{id: "tether"}}, origins...) {
		cursor := ""
		seen := map[string]bool{}
		for {
			if len(seen) >= 100 {
				return nil, nil, resourceInventoryError()
			}
			var result mcpsdk.Result
			var err error
			if templates {
				params := &mcpsdk.ListResourceTemplatesParams{Meta: meta, Cursor: cursor}
				if origin.client != nil {
					params.Meta = resourceRequestMeta(ctx, params.Meta)
					result, err = origin.client.ListResourceTemplates(ctx, params)
				} else {
					original := req.(*mcpsdk.ListResourceTemplatesRequest)
					copied := *original
					copied.Params = params
					result, err = next(ctx, "resources/templates/list", &copied)
				}
			} else {
				params := &mcpsdk.ListResourcesParams{Meta: meta, Cursor: cursor}
				if origin.client != nil {
					params.Meta = resourceRequestMeta(ctx, params.Meta)
					result, err = origin.client.ListResources(ctx, params)
				} else {
					original := req.(*mcpsdk.ListResourcesRequest)
					copied := *original
					copied.Params = params
					result, err = next(ctx, "resources/list", &copied)
				}
			}
			if err != nil {
				return nil, nil, resourceInventoryError()
			}
			var pageMeta mcpsdk.Meta
			if templates {
				page, ok := result.(*mcpsdk.ListResourceTemplatesResult)
				if !ok || page == nil {
					return nil, nil, resourceInventoryError()
				}
				for _, item := range page.ResourceTemplates {
					if item != nil {
						records = append(records, resourceRecord{origin: origin, template: item})
					}
				}
				cursor, pageMeta = page.NextCursor, page.Meta
			} else {
				page, ok := result.(*mcpsdk.ListResourcesResult)
				if !ok || page == nil {
					return nil, nil, resourceInventoryError()
				}
				for _, item := range page.Resources {
					if item != nil {
						records = append(records, resourceRecord{origin: origin, resource: item})
					}
				}
				cursor, pageMeta = page.NextCursor, page.Meta
			}
			if len(pageMeta) > 0 {
				pages, _ := upstreamMeta[origin.id].([]mcpsdk.Meta)
				upstreamMeta[origin.id] = append(pages, pageMeta)
			}
			if len(records) > 10000 || cursor != "" && seen[cursor] {
				return nil, nil, resourceInventoryError()
			}
			if cursor == "" {
				break
			}
			seen[cursor] = true
		}
	}
	resultMeta := mcpsdk.Meta{}
	if len(upstreamMeta) > 0 {
		resultMeta["tether/upstream"] = upstreamMeta
	}
	return records, resultMeta, nil
}

// Aggregate cursors bind the visible inventory and policy; upstream cursors
// never cross origins or become caller-controlled routing instructions.
func resourcePage[T any](items []T, cursor string, policy *mcpgateway.Policy) ([]T, string, error) {
	raw, err := json.Marshal(struct {
		Items  []T
		Policy *mcpgateway.Policy
	}{items, policy})
	if err != nil {
		return nil, "", err
	}
	binding := fmt.Sprintf("%x", sha256.Sum256(raw))
	offset := 0
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		var page struct {
			Binding string
			Offset  int
		}
		if err != nil || json.Unmarshal(decoded, &page) != nil || page.Binding != binding || page.Offset < 0 || page.Offset >= len(items) {
			return nil, "", &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid or stale resource cursor"}
		}
		offset = page.Offset
	}
	end := min(offset+mcpsdk.DefaultPageSize, len(items))
	continuation := ""
	if end < len(items) {
		encoded, _ := json.Marshal(struct {
			Binding string
			Offset  int
		}{binding, end})
		continuation = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return items[offset:end], continuation, nil
}

func (a *Adapter) resourceProxyMiddleware(pool *ClientPool, allowed map[string]bool, gateway *mcpgateway.Service, publisher events.Publisher, liveServers ...func() ([]string, error)) mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (result mcpsdk.Result, err error) {
			if method != "resources/list" && method != "resources/templates/list" && method != "resources/read" {
				return next(ctx, method, req)
			}
			ctx = a.withSessionID(ctx)
			policy := gateway.Policy
			if policy == nil {
				policy = &mcpgateway.Policy{}
			}
			snapshot := gateway.Snapshot()
			liveAllowed := allowed
			if len(liveServers) > 0 && liveServers[0] != nil {
				ids, err := liveServers[0]()
				liveAllowed = map[string]bool{}
				if err == nil {
					for _, id := range ids {
						if allowed[id] {
							liveAllowed[id] = true
						}
					}
				}
			}
			permits := func(origin, uri string) bool {
				if origin == "tether" {
					return true
				} // docsResourceMiddleware applies native tool policy
				if docsResourceTool(uri) != "" {
					return false
				} // native namespace cannot be shadowed
				return (liveAllowed == nil || liveAllowed[origin]) && policy.ResourceAllowed(origin, uri, snapshot)
			}
			origins := []resourceOrigin{}
			for _, origin := range pool.resourceOrigins(liveAllowed) {
				if policy.ResourceOriginAllowed(origin.id) {
					origins = append(origins, origin)
				}
			}
			if method == "resources/read" {
				read, ok := req.(*mcpsdk.ReadResourceRequest)
				if !ok || read.Params == nil {
					return nil, resourceUnavailable()
				}
				audit := telemetry.Service{Publisher: publisher, Secrets: proxyRedactionSet(pool.entries)}
				var observation *telemetry.Observation
				ctx, observation = audit.Start(ctx, telemetry.Call{Name: "resources/read", Server: "gateway", SessionID: sessionIDFromContext(ctx), Profile: policy.Selection.ID, Mode: string(gateway.Selection.Mode), Fingerprint: argsSchemaFP([]byte(`{"uri":""}`))})
				defer func() {
					out := telemetry.Outcome{OK: err == nil}
					if err != nil {
						out.Error = "resource read failed"
						out.Class = events.ToolErrorDenied
					}
					if result != nil {
						out.ResultBytes, _ = telemetry.JSONSize(result)
					}
					audit.End(ctx, observation, out)
				}()
				// Native documentation URIs retain their native handler and
				// policy even if an upstream attempts to advertise the URI.
				if docsResourceTool(read.Params.URI) != "" {
					return next(ctx, method, req)
				}
				// Resolve only advertised exact URIs or URI templates. Never fetch an
				// arbitrary URL, infer an origin from a scheme, or try origins in order.
				listReq := &mcpsdk.ListResourcesRequest{Session: read.Session, Extra: read.Extra, Params: &mcpsdk.ListResourcesParams{Meta: read.Params.Meta}}
				records, _, catalogErr := resourceCatalog(ctx, origins, read.Params.Meta, false, next, listReq)
				if catalogErr != nil {
					telemetry.SetErrorClass(ctx, events.ToolErrorUpstreamDown)
					return nil, catalogErr
				}
				templateReq := &mcpsdk.ListResourceTemplatesRequest{Session: read.Session, Extra: read.Extra, Params: &mcpsdk.ListResourceTemplatesParams{Meta: read.Params.Meta}}
				templates, _, catalogErr := resourceCatalog(ctx, origins, read.Params.Meta, true, next, templateReq)
				if catalogErr != nil {
					telemetry.SetErrorClass(ctx, events.ToolErrorUpstreamDown)
					return nil, catalogErr
				}
				owners := map[string]resourceOrigin{}
				for _, record := range append(records, templates...) {
					matches := record.resource != nil && record.resource.URI == read.Params.URI
					if record.template != nil {
						template, parseErr := uritemplate.New(record.template.URITemplate)
						matches = parseErr == nil && template.Regexp().MatchString(read.Params.URI)
					}
					if matches {
						owners[record.origin.id] = record.origin
					}
				}
				if len(owners) != 1 {
					return nil, resourceUnavailable()
				}
				for _, owner := range owners {
					if !permits(owner.id, read.Params.URI) {
						return nil, resourceUnavailable()
					}
					finish := telemetry.Forward(ctx)
					defer finish()
					if owner.client == nil {
						return next(ctx, method, req)
					}
					params := *read.Params
					params.Meta = resourceRequestMeta(ctx, params.Meta)
					value, readErr := owner.client.ReadResource(ctx, &params)
					if readErr != nil {
						telemetry.SetErrorClass(ctx, events.ToolErrorUpstream)
						return nil, resourceUnavailable()
					}
					return value, nil
				}
				return nil, resourceUnavailable()
			}
			if method == "resources/list" {
				list := req.(*mcpsdk.ListResourcesRequest)
				params := list.Params
				if params == nil {
					params = &mcpsdk.ListResourcesParams{}
				}
				records, meta, listErr := resourceCatalog(ctx, origins, params.Meta, false, next, list)
				if listErr != nil {
					return nil, listErr
				}
				items := []*mcpsdk.Resource{}
				visible := map[string]bool{}
				for _, record := range unambiguousResources(records) {
					if permits(record.origin.id, record.resource.URI) {
						items = append(items, record.resource)
						visible[record.origin.id] = true
					}
				}
				sort.SliceStable(items, func(i, j int) bool { return items[i].URI < items[j].URI })
				items, cursor, pageErr := resourcePage(items, params.Cursor, policy)
				return &mcpsdk.ListResourcesResult{Resources: items, NextCursor: cursor, Meta: visibleResourceMeta(meta, visible)}, pageErr
			}
			list := req.(*mcpsdk.ListResourceTemplatesRequest)
			params := list.Params
			if params == nil {
				params = &mcpsdk.ListResourceTemplatesParams{}
			}
			records, meta, listErr := resourceCatalog(ctx, origins, params.Meta, true, next, list)
			if listErr != nil {
				return nil, listErr
			}
			items := []*mcpsdk.ResourceTemplate{}
			visible := map[string]bool{}
			for _, record := range unambiguousResources(records) {
				if permits(record.origin.id, record.template.URITemplate) {
					items = append(items, record.template)
					visible[record.origin.id] = true
				}
			}
			sort.SliceStable(items, func(i, j int) bool { return items[i].URITemplate < items[j].URITemplate })
			items, cursor, pageErr := resourcePage(items, params.Cursor, policy)
			return &mcpsdk.ListResourceTemplatesResult{ResourceTemplates: items, NextCursor: cursor, Meta: visibleResourceMeta(meta, visible)}, pageErr
		}
	}
}

// Fan-in has no single upstream result metadata owner. Preserve each origin's
// page metadata in a namespace, exposing it only when that origin is visible.
func visibleResourceMeta(meta mcpsdk.Meta, visible map[string]bool) mcpsdk.Meta {
	pages, _ := meta["tether/upstream"].(map[string]any)
	filtered := map[string]any{}
	for origin, value := range pages {
		if visible[origin] {
			filtered[origin] = value
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return mcpsdk.Meta{"tether/upstream": filtered}
}

// Do not publish duplicate URI identities from different admitted origins.
// Reads independently check exact and overlapping template ownership.
func unambiguousResources(records []resourceRecord) []resourceRecord {
	owners := map[string]string{}
	ambiguous := map[string]bool{}
	key := func(record resourceRecord) string {
		if record.resource != nil {
			return record.resource.URI
		}
		return record.template.URITemplate
	}
	// Native documentation owns its namespace before collision resolution.
	// An upstream advertisement must not hide a registered native resource.
	eligible := make([]resourceRecord, 0, len(records))
	for _, record := range records {
		if record.origin.id == "tether" || docsResourceTool(key(record)) == "" {
			eligible = append(eligible, record)
		}
	}
	records = eligible
	for _, record := range records {
		uri := key(record)
		if owner, exists := owners[uri]; exists && owner != record.origin.id {
			ambiguous[uri] = true
		}
		owners[uri] = record.origin.id
	}
	out := make([]resourceRecord, 0, len(records))
	seen := map[string]bool{}
	for _, record := range records {
		uri := key(record)
		if !ambiguous[uri] && !seen[uri] {
			out = append(out, record)
			seen[uri] = true
		}
	}
	return out
}
