package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Optional so tools-only test doubles and old upstreams remain valid.
type resourceClient interface {
	ListResources(context.Context, *mcpsdk.ListResourcesParams) (*mcpsdk.ListResourcesResult, error)
	ListResourceTemplates(context.Context, *mcpsdk.ListResourceTemplatesParams) (*mcpsdk.ListResourceTemplatesResult, error)
	ReadResource(context.Context, *mcpsdk.ReadResourceParams) (*mcpsdk.ReadResourceResult, error)
	InitializeResult() *mcpsdk.InitializeResult
}

func (r *remoteClient) resourceSession(ctx context.Context) (*mcpsdk.ClientSession, error) {
	if err := r.gc.Ping(ctx); err != nil {
		return nil, err
	}
	if session := r.gc.SDKSession(); session != nil {
		return session, nil
	}
	return nil, fmt.Errorf("upstream resource session unavailable")
}

// Resource metadata and contents can echo the daemon-only credential too.
func scrubResourceResult[T any](value *T, secret string) (*T, error) {
	if value == nil || secret == "" {
		return value, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("upstream resource result could not be scrubbed")
	}
	if !bytes.Contains(raw, []byte(secret)) {
		return value, nil
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("upstream resource result could not be scrubbed")
	}
	raw, err = json.Marshal(scrubServiceValue(decoded, secret))
	if err != nil {
		return nil, fmt.Errorf("upstream resource result could not be scrubbed")
	}
	var result T
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("upstream resource result could not be scrubbed")
	}
	return &result, nil
}
func (r *remoteClient) ListResources(ctx context.Context, params *mcpsdk.ListResourcesParams) (*mcpsdk.ListResourcesResult, error) {
	session, err := r.resourceSession(ctx)
	if err != nil {
		return nil, err
	}
	result, err := session.ListResources(withForwardedToolCall(ctx), params)
	if err != nil {
		return nil, err
	}
	return scrubResourceResult(result, r.serviceToken)
}
func (r *remoteClient) ListResourceTemplates(ctx context.Context, params *mcpsdk.ListResourceTemplatesParams) (*mcpsdk.ListResourceTemplatesResult, error) {
	session, err := r.resourceSession(ctx)
	if err != nil {
		return nil, err
	}
	result, err := session.ListResourceTemplates(withForwardedToolCall(ctx), params)
	if err != nil {
		return nil, err
	}
	return scrubResourceResult(result, r.serviceToken)
}
func (r *remoteClient) ReadResource(ctx context.Context, params *mcpsdk.ReadResourceParams) (*mcpsdk.ReadResourceResult, error) {
	session, err := r.resourceSession(ctx)
	if err != nil {
		return nil, err
	}
	result, err := session.ReadResource(withForwardedToolCall(ctx), params)
	if err != nil {
		return nil, err
	}
	return scrubResourceResult(result, r.serviceToken)
}

// Reuse the trusted outbound metadata boundary: callers cannot invent Tether
// context/provenance on resource requests, and tracing stays out of parameters.
func resourceRequestMeta(ctx context.Context, meta mcpsdk.Meta) mcpsdk.Meta {
	params := &mcpsdk.CallToolParams{Meta: mcpsdk.Meta(stripSDKMeta(meta))}
	router := &ProxyRouter{}
	router.applyProvenanceMeta(ctx, params)
	injectTraceContextMeta(ctx, params)
	return params.Meta
}
