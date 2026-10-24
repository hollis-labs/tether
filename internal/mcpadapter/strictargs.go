package mcpadapter

import (
	"context"
	"fmt"
	"sort"
	"strings"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
)

// gatewayMetadataKeys are the trace keys the proxy still writes into a call's
// arguments (CW-20260907-0026 moves them to params._meta; older muxes keep
// writing here). They are ignored by the undeclared-argument check. Any other
// underscore-prefixed name is refused: that is not an escape hatch.
var gatewayMetadataKeys = map[string]struct{}{
	"_traceparent": {},
	"_tracestate":  {},
}

// payloadLikeArgs are the names callers reach for when they mean the message
// body. Naming them in the refusal turns "unknown argument" into the fix.
var payloadLikeArgs = map[string]struct{}{
	"subject": {}, "body": {}, "text": {}, "message": {}, "content": {}, "payload": {},
}

// strictArgs wraps t's handler so a call naming an argument the tool's own
// schema does not declare is refused instead of accepted and ignored. A name
// the handler never reads used to arrive, be dropped, and return ok:true --
// for the message tools that stored payload:null, a blank mail body
// (CW-20261024-0040; the same drop as CW-20260521-0018, the top-level
// subject/body shape).
//
// The accepted set is read off the schema at registration, so there is no
// second list to keep in step. Opt-in per tool: a Tether-wide switch is the
// portfolio audit's decision (CW-20261024-0039).
func strictArgs(t gomcp.Tool) gomcp.Tool {
	declared := declaredArgNames(t.InputSchema)
	if len(declared) == 0 {
		panic("mcpadapter: strictArgs on " + t.Name + ": schema declares no arguments")
	}
	inner := t.Handler
	t.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		if err := rejectUndeclaredArgs(t.Name, declared, args); err != nil {
			return nil, err
		}
		return inner(ctx, args)
	}
	return t
}

func declaredArgNames(schema any) map[string]struct{} {
	out := map[string]struct{}{}
	m, _ := schema.(map[string]interface{})
	props, _ := m["properties"].(map[string]interface{})
	for name := range props {
		out[name] = struct{}{}
	}
	return out
}

func rejectUndeclaredArgs(tool string, declared map[string]struct{}, args map[string]any) error {
	var bad []string
	for k := range args {
		if _, ok := declared[k]; ok {
			continue
		}
		if _, ok := gatewayMetadataKeys[k]; ok {
			continue
		}
		bad = append(bad, k)
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	accepted := make([]string, 0, len(declared))
	for k := range declared {
		accepted = append(accepted, k)
	}
	sort.Strings(accepted)

	msg := fmt.Sprintf("%s does not accept argument(s) %s; nothing was sent. Accepted: %s.",
		tool, strings.Join(bad, ", "), strings.Join(accepted, ", "))
	for _, k := range bad {
		if _, ok := payloadLikeArgs[k]; ok {
			msg += ` The message body goes inside payload_json, e.g. payload_json="{\"subject\":\"...\",\"body\":\"...\"}".`
			break
		}
	}
	return toolError("invalid_request", msg)
}
