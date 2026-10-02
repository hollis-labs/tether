package mcpgateway

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"testing"
)

func TestKeywordScorePreservesExactBonusTokensAndTags(t *testing.T) {
	entry := Entry{Tool: &mcpsdk.Tool{Name: "Foo_BAR", Description: "Fetch records; records"}, Tags: []string{"STORAGE", "multi-word"}}
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"Foo_BAR", 3}, {"foo_bar", 2}, {"records RECORDS", 1}, {"storage", 1}, {"multi word", 0}, {"fetch foo storage", 3}, {"f 7 中文", 0}, {"unknown", 0},
	} {
		if got := KeywordScore(tc.query, entry); got != tc.want {
			t.Errorf("query=%q score=%d want=%d", tc.query, got, tc.want)
		}
	}
	single := Entry{Tool: &mcpsdk.Tool{Name: "x"}}
	if got := KeywordScore("x", single); got != 1 {
		t.Fatalf("exact single-character score=%d", got)
	}
}
