package mcpadapter

// trace_meta.go — where W3C trace context rides on an MCP call.
//
// CW-20260907-0026. MCP puts request metadata in `params._meta`, not in
// `params.arguments`. Tether used to inject into arguments, which any upstream
// declaring `additionalProperties: false` at its schema root correctly
// rejected:
//
//	validating "arguments": validating root: unexpected additional properties ["_traceparent"]
//
// That made every tangent.hitl_* tool uncallable on the default config until
// Tangent added a strip at its own boundary, and it put Tesseract in the same
// position the moment it made its schemas strict. Both strips exist to
// accommodate a gateway defect; this file is the fix they are waiting on.

import (
	otelprop "github.com/hollis-labs/go-otel/propagation"
	"github.com/mark3labs/mcp-go/mcp"

	"context"
)

// traceMetaKeys are the keys go-otel's propagation helpers read and write.
// Named here only so the dual-read below can tell whether a legacy call
// actually carried trace context, without duplicating go-otel's format.
var traceMetaKeys = []string{"_traceparent", "_tracestate"}

// injectTraceContextMeta returns req with trace context written into
// params._meta, leaving params.arguments untouched.
//
// KEY NAMING. go-otel writes the literal key `_traceparent`, so this produces
// `params._meta._traceparent`. The leading underscore was the marker for
// "metadata smuggled into arguments" and is redundant inside _meta, where
// everything is metadata by definition. It is kept deliberately: bare
// `traceparent` would mean either changing go-otel's exported helpers --
// whose consumers include Hadron, Loom and Tangent, making it a four-repo
// change -- or hand-writing the keys here and diverging from how every other
// consumer in the portfolio injects. A cosmetic redundancy is the cheaper
// price. This is a choice, not an oversight.
//
// InjectMCP no-ops on an invalid span context, so a call made with no active
// span comes back with no _meta added rather than an empty one.
func injectTraceContextMeta(ctx context.Context, req mcp.CallToolRequest) mcp.CallToolRequest {
	fields := map[string]any{}
	if req.Params.Meta != nil && req.Params.Meta.AdditionalFields != nil {
		fields = req.Params.Meta.AdditionalFields
	}
	injected := otelprop.InjectMCP(ctx, fields)
	if len(injected) == 0 {
		return req
	}
	if req.Params.Meta == nil {
		req.Params.Meta = &mcp.Meta{}
	}
	req.Params.Meta.AdditionalFields = injected
	return req
}

// extractTraceContext recovers a remote span context from an inbound call,
// reading _meta first and falling back to arguments.
//
// THE ARGUMENTS FALLBACK IS A TRANSITION, AND IT HAS A SCHEDULED END:
// CW-20260912-0072, which carries the condition (every deployed mux past
// CW-20260907-0026) rather than a date. It is here because every mux built
// before that change still writes to arguments, and a newer mux receiving a
// call from an older one would otherwise silently lose the parent span --
// silently, because a broken parent link and a genuinely-new trace are
// indistinguishable downstream.
//
// Reading arguments is safe in a way WRITING them was not: reading an
// upstream's own key cannot make a call fail schema validation.
func extractTraceContext(req mcp.CallToolRequest) context.Context {
	if req.Params.Meta != nil && hasTraceKeys(req.Params.Meta.AdditionalFields) {
		return otelprop.ExtractMCP(req.Params.Meta.AdditionalFields)
	}
	return otelprop.ExtractMCP(req.GetArguments())
}

// hasTraceKeys reports whether m carries trace context, so an empty _meta
// (a progressToken and nothing else, say) falls through to the legacy
// location instead of being treated as an authoritative empty answer.
func hasTraceKeys(m map[string]any) bool {
	for _, k := range traceMetaKeys {
		if v, ok := m[k].(string); ok && v != "" {
			return true
		}
	}
	return false
}
