package mcpadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// A long SVG or HTML document must reach a tool with its root close tag intact
// (CW-20261024-0130). Before go-mcp's sanitizer was fixed (plugin-mcp v0.6.0) the
// request-path middleware deleted a trailing </svg> or </html> whenever the opener
// was 64 or more characters earlier. Stray close tags are still stripped.
func TestSanitizerKeepsTheRootCloseTagOfLongDocumentsOnTheRequestPath(t *testing.T) {
	a := newTestAdapter(t)
	s := a.newBareServer()
	a.addTool(s, gomcp.Tool{
		Name:        "echo_content",
		Description: "Echoes its content argument as the server received it.",
		InputSchema: gomcp.InputSchema(gomcp.StringProp("content", "free text", true)),
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			return toolJSON(map[string]any{"content": args["content"]}), nil
		},
	}, Reads("test echo"))
	c := connectInMemory(t, s)

	echo := func(in string) string {
		t.Helper()
		res, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "echo_content", Arguments: map[string]any{"content": in}})
		if err != nil || res.IsError {
			t.Fatalf("call: %v %v", res, err)
		}
		var out struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(res.Content[0].(*mcpsdk.TextContent).Text), &out); err != nil {
			t.Fatal(err)
		}
		return out.Content
	}

	svg := `<svg xmlns="http://www.w3.org/2000/svg" width="40" height="20"><rect width="40" height="20" fill="#06b6d4"/></svg>`
	html := "<!doctype html><html><head><title>t</title></head><body>" + strings.Repeat("<p>paragraph of content</p>", 10) + "</body></html>"
	for name, doc := range map[string]string{"svg": svg, "html": html} {
		if got := echo(doc); got != doc {
			t.Errorf("%s lost its closing tag: got %d bytes, sent %d", name, len(got), len(doc))
		}
	}
	// A close tag with no opener is still leakage and is still removed.
	if got := echo("prose that ends with a stray tag\n</note>"); strings.Contains(got, "</note>") {
		t.Errorf("a stray close tag should still be stripped: %q", got)
	}
}
