package mcpgateway

import "testing"

// BenchmarkSearchScorers measures one search over the pinned 725-tool catalog.
// BM25 builds its index on every call, so this is its per-request cost.
func BenchmarkSearchScorers(b *testing.B) {
	snap := loadEvalCatalog(b).snapshot()
	for _, sc := range searchEvalScorers {
		b.Run(sc.Name, func(b *testing.B) {
			svc := &Service{Snapshot: func() Snapshot { return snap }, Score: sc.Score, ScoreAll: sc.ScoreAll}
			for i := 0; i < b.N; i++ {
				if _, err := svc.Search(Request{Query: "send a message to another agent"}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
