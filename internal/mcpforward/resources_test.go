package mcpforward

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestForwardResourcesPreservesUIAndDenials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	daemon := mcp.NewServer(&mcp.Implementation{Name: "resource-daemon", Version: "test"}, nil)
	const uri = "ui://fixture/card.html"
	read := func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		if req.Params.Meta["fixture"] != "metadata" {
			t.Error("request metadata lost")
		}
		return &mcp.ReadResourceResult{Meta: mcp.Meta{"fixture": "result"}, Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/html;profile=mcp-app", Text: "<html>card</html>", Meta: mcp.Meta{"ui": map[string]any{"prefersBorder": true}}}}}, nil
	}
	daemon.AddResource(&mcp.Resource{URI: uri, Name: "card", Meta: mcp.Meta{"fixture": "resource"}}, read)
	daemon.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "ui://fixture/{name}.html", Name: "card", Meta: mcp.Meta{"fixture": "template"}}, read)
	daemon.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if read, ok := req.(*mcp.ReadResourceRequest); ok && read.Params.URI == "ui://fixture/denied.html" {
				return nil, &jsonrpc.Error{Code: -32002, Message: "denied resource"}
			}
			return next(ctx, method, req)
		}
	})
	remote := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil))
	defer remote.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	left, right := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, client.New("tcp:"+strings.TrimPrefix(remote.URL, "http://"), client.WithToken("test-session")), client.MCPOptions{}, right)
	}()
	consumer, err := mcp.NewClient(&mcp.Implementation{Name: "resource-consumer", Version: "test"}, nil).Connect(ctx, left, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	if consumer.InitializeResult().Capabilities.Resources == nil {
		t.Fatal("resource capability absent")
	}
	resources, err := consumer.ListResources(ctx, nil)
	if err != nil || len(resources.Resources) != 1 || resources.Resources[0].URI != uri || resources.Resources[0].Meta["fixture"] != "resource" {
		t.Fatalf("resources lost: %+v %v", resources, err)
	}
	templates, err := consumer.ListResourceTemplates(ctx, nil)
	if err != nil || len(templates.ResourceTemplates) != 1 || templates.ResourceTemplates[0].Meta["fixture"] != "template" {
		t.Fatalf("templates lost: %+v %v", templates, err)
	}
	for _, uri := range []string{uri, "ui://fixture/other.html"} {
		result, err := consumer.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri, Meta: mcp.Meta{"fixture": "metadata"}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Meta["fixture"] != "result" || result.Contents[0].Text != "<html>card</html>" || result.Contents[0].Meta["ui"] == nil {
			t.Fatal("resource body/metadata lost")
		}
	}
	if _, err := consumer.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ui://fixture/denied.html"}); err == nil || !strings.Contains(err.Error(), "denied resource") {
		t.Fatalf("denial lost: %v", err)
	}
	_ = consumer.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("forwarder did not close")
	}
}
