package mcpgateway

import (
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func bm25Entries() []Entry {
	mk := func(name, desc string, tags ...string) Entry {
		return Entry{Tool: &mcpsdk.Tool{Name: name, Description: desc}, Origin: "o", Tags: tags}
	}
	return []Entry{
		mk("mail_send", "Send a message to another agent."),
		mk("mail_list", "List messages in an inbox."),
		mk("disk_usage", "Report disk usage.", "storage"),
	}
}

func TestBM25ScoreAllRanksAndExcludes(t *testing.T) {
	entries := bm25Entries()
	got := BM25ScoreAll("send message inbox", entries)
	if len(got) != len(entries) {
		t.Fatalf("scores=%d entries=%d", len(got), len(entries))
	}
	if got[0] <= got[1] || got[1] <= 0 {
		t.Fatalf("want send > list > 0, got %v", got)
	}
	if got[2] != 0 {
		t.Fatalf("unrelated tool scored %d, want 0 (excluded)", got[2])
	}
	if exact := BM25ScoreAll("disk_usage", entries); exact[2] <= exact[0] {
		t.Fatalf("exact name must rank first: %v", exact)
	}
	if none := BM25ScoreAll("zebra", entries); none[0]+none[1]+none[2] != 0 {
		t.Fatalf("no-match query scored %v", none)
	}
}

func TestBM25ScoreAllFallsBackOnDuplicateNames(t *testing.T) {
	e := bm25Entries()
	e[1] = e[0]
	got := BM25ScoreAll("send message", e)
	want := keywordScoreAll("send message", e)
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("fallback differs: got %v want %v", got, want)
		}
	}
}

func TestUseScorerSwitch(t *testing.T) {
	var s Service
	s.UseScorer("")
	if s.ScoreAll == nil || s.Score != nil {
		t.Fatal("default must be BM25")
	}
	s.UseScorer(" Keyword ")
	if s.Score == nil || s.ScoreAll != nil {
		t.Fatal("keyword must restore the per-entry scorer")
	}
	s.UseScorer("anything-else")
	if s.ScoreAll == nil || s.Score != nil {
		t.Fatal("unknown name must fall to BM25")
	}
}

func TestServiceRejectsScoreAllLengthMismatch(t *testing.T) {
	snap := Snapshot{Entries: bm25Entries(), Origins: []OriginStatus{{ID: "o", Status: "connected"}}}
	s := &Service{Snapshot: func() Snapshot { return snap }, ScoreAll: func(string, []Entry) []int { return []int{1} }}
	if _, err := s.Search(Request{Query: "send"}); err == nil {
		t.Fatal("a scorer returning the wrong number of scores must fail, not misrank")
	}
}
