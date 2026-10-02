package telemetry

import (
	"encoding/json"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"strings"
	"testing"
)

func TestJSONSizeMatchesCanonicalEncoding(t *testing.T) {
	for _, value := range []any{nil, map[string]any{}, map[string]any{"text": "€<&\n"}, &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: strings.Repeat("x", 1<<20)}}}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		size, err := JSONSize(value)
		if err != nil || size != int64(len(raw)) {
			t.Fatalf("size %d != %d / %v", size, len(raw), err)
		}
	}
	if _, err := JSONSize(make(chan int)); err == nil {
		t.Fatal("unencodable value accepted")
	}
}
func BenchmarkResultByteCounting(b *testing.B) {
	result := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: strings.Repeat("x", 1<<20)}}}
	b.Run("MarshalBaseline", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			raw, err := json.Marshal(result)
			if err != nil || len(raw) == 0 {
				b.Fatal(err)
			}
		}
	})
	b.Run("CountingWriter", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			size, err := JSONSize(result)
			if err != nil || size == 0 {
				b.Fatal(err)
			}
		}
	})
}
