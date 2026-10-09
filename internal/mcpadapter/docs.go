package mcpadapter

import (
	"context"
	"encoding/json"
	"strings"

	gomcp "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (a *Adapter) docs() *app.DocsService { return a.svc.Docs() }

func (a *Adapter) registerDocsTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{Name: "tether_docs_list", Description: "List the running Tether binary's MCP guides: titles and one-line usage triggers only. Choose an id, then use tether_docs_get for instructions and its reference-file manifest.", InputSchema: gomcp.EmptyObjectSchema(), Handler: func(context.Context, map[string]any) (any, error) { return a.docs().List(), nil }}, Reads("embedded documentation metadata"))
	a.addTool(s, gomcp.Tool{Name: "tether_docs_get", Description: "Read one embedded MCP guide and its reference-file manifest. Use an id from tether_docs_list; fetch a declared reference with tether_docs_get_file.", InputSchema: gomcp.InputSchema(gomcp.StringProp("id", "Guide id from tether_docs_list", true)), Handler: func(_ context.Context, args map[string]any) (any, error) { return a.docs().Get(str(args, "id")) }}, Reads("embedded documentation body and reference metadata"))
	a.addTool(s, gomcp.Tool{Name: "tether_docs_get_file", Description: "Read one embedded reference file declared in a guide's manifest. Supply its guide id and exact manifest path; arbitrary host paths and URLs are not supported.", InputSchema: gomcp.InputSchema(gomcp.StringProp("id", "Guide id whose manifest declares the reference", true), gomcp.StringProp("path", "Exact path returned in the guide's files manifest", true)), Handler: func(_ context.Context, args map[string]any) (any, error) {
		return a.docs().GetFile(str(args, "id"), str(args, "path"))
	}}, Reads("embedded documentation reference file"))
}

// Register resources on the public server too: native tool forwarding alone
// does not forward the native server's resources/list or resources/read.
func (a *Adapter) registerDocsResources(s *gomcp.Server) {
	seen := map[string]bool{}
	for _, meta := range a.docs().List() {
		s.SDKServer().AddResource(&mcpsdk.Resource{URI: meta.URI, Name: meta.ID, Title: meta.Title, Description: meta.Trigger, MIMEType: "application/json"}, func(context.Context, *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
			doc, err := a.docs().Get(meta.ID)
			if err != nil {
				return nil, err
			}
			body, err := json.Marshal(doc)
			if err != nil {
				return nil, err
			}
			return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: meta.URI, MIMEType: "application/json", Text: string(body)}}}, nil
		})
		doc, err := a.docs().Get(meta.ID)
		if err != nil {
			continue
		}
		for _, file := range doc.Files {
			if seen[file.URI] {
				continue
			}
			seen[file.URI] = true
			s.SDKServer().AddResource(&mcpsdk.Resource{URI: file.URI, Name: file.Path, Description: "Embedded reference file; discover its context through a guide manifest.", MIMEType: file.MIMEType}, func(context.Context, *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
				body, err := a.docs().GetFile(meta.ID, file.Path)
				if err != nil {
					return nil, err
				}
				return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{{URI: file.URI, MIMEType: file.MIMEType, Text: body.Body}}}, nil
			})
		}
	}
}

func docsResourceTool(uri string) string {
	if strings.HasPrefix(uri, "tether://docs/mcp/") {
		return "tether_docs_get"
	}
	if strings.HasPrefix(uri, "tether://docs/files/") {
		return "tether_docs_get_file"
	}
	return ""
}

// Resources share the corresponding tool's eligible target boundary in both
// flat and search modes; resource support cannot bypass a native/tool denial.
func docsResourceMiddleware(gateway *mcpgateway.Service) mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			allowed := func(uri string) bool {
				name := docsResourceTool(uri)
				if name == "" {
					return true
				}
				_, err := gateway.ResolveTarget(name)
				return err == nil
			}
			if read, ok := req.(*mcpsdk.ReadResourceRequest); ok && !allowed(read.Params.URI) {
				return nil, &jsonrpc.Error{Code: -32002, Message: "documentation resource unavailable under this profile"}
			}
			result, err := next(ctx, method, req)
			if err != nil {
				return nil, err
			}
			if listed, ok := result.(*mcpsdk.ListResourcesResult); ok {
				visible := make([]*mcpsdk.Resource, 0, len(listed.Resources))
				for _, resource := range listed.Resources {
					if allowed(resource.URI) {
						visible = append(visible, resource)
					}
				}
				listed.Resources = visible
			}
			return result, nil
		}
	}
}
