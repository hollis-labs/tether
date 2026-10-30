package mcpgateway

// Search-eval harness (CW-20261024-0056): scores tool discovery against a
// versioned, human-reviewable fixture so a scorer change (BM25, code mode) is
// judged by numbers rather than by feel.
//
//	make search-eval                     # print recall@1/5/10, MRR, median bytes
//	go test ./internal/mcpgateway -run TestSearchEval -count=1 -args -search-eval-update
//	go test ./internal/mcpgateway -run TestSearchEvalSnapshot -count=1 -args -search-eval-snapshot-from=<inventory.json>
//
// Fixture files live in testdata/searcheval/: catalog.json (pinned tool
// snapshot), queries.json (labeled queries) and baseline_<scorer>.json
// (committed numbers). A new scorer plugs in by adding one entry to
// searchEvalScorers; see docs/agents/mcp/discovery.md.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	searchEvalUpdate       = flag.Bool("search-eval-update", false, "rewrite the committed search-eval baselines from the current scorers")
	searchEvalSnapshotFrom = flag.String("search-eval-snapshot-from", "", "regenerate testdata/searcheval/catalog.json from a gateway inventory dump (JSON array of tools)")
)

const searchEvalDir = "testdata/searcheval"

// searchEvalScorers are the scorers under evaluation. The first is the
// production scorer; add a scorer here (Score for per-entry, ScoreAll for corpus-relative).
var searchEvalScorers = []struct {
	Name     string
	Score    func(string, Entry) int
	ScoreAll func(string, []Entry) []int
}{
	{"keyword", KeywordScore, nil},
	{"bm25", nil, BM25ScoreAll},
}

// searchEvalOriginTags are the tags the gateway attaches to each origin
// (from tether_health); the inventory dump does not carry them.
var searchEvalOriginTags = map[string][]string{
	"caster":              {"notes", "scratchpad", "drafts", "refs", "cas", "caster"},
	"cerberus":            {"services", "builds", "lifecycle"},
	"fragments-engine":    {"fragments", "ingest", "routing", "recall", "fragments-engine"},
	"hadron":              {"automation", "ci", "blueprints", "pipelines"},
	"loom":                {"content", "wiki", "docs", "compile", "loom"},
	"nanite":              {"notes", "chat", "search", "context"},
	"tachyon":             {"tachyon", "planner"},
	"tachyon_pa":          {"tachyon", "planner"},
	"tachyon_pm":          {"tachyon", "planner"},
	"tachyon-actions-aom": {"tachyon", "actions"},
	"tachyon-actions-fom": {"tachyon", "actions"},
	"tangent":             {"interactive-ui", "workflows", "review", "tangent"},
	"tesseract":           {"memory", "knowledge", "context", "tesseract"},
	"torque":              {"tasks", "sprints", "planning", "orchestration", "torque"},
}

type evalTool struct {
	Name        string                  `json:"name"`
	Origin      string                  `json:"origin"`
	Title       string                  `json:"title,omitempty"`
	Description string                  `json:"description"`
	Annotations *mcpsdk.ToolAnnotations `json:"annotations,omitempty"`
	Required    []string                `json:"required,omitempty"`
	// Arguments are the tool's top-level input-schema properties (CW-20261024-0115).
	Arguments []evalArgument `json:"arguments,omitempty"`
}

type evalArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type evalCatalog struct {
	Schema   int        `json:"schema"`
	Captured string     `json:"captured"`
	Source   string     `json:"source"`
	Tools    int        `json:"tools"`
	SHA256   string     `json:"sha256"`
	Entries  []evalTool `json:"entries"`
}

type evalQuery struct {
	ID         string   `json:"id"`
	Query      string   `json:"query"`
	Expect     []string `json:"expect"`
	Category   string   `json:"category"`
	UsageCalls int      `json:"usage_calls,omitempty"`
	Note       string   `json:"note,omitempty"`
	Review     bool     `json:"review,omitempty"`
}

type evalQuerySet struct {
	Schema   int         `json:"schema"`
	Labeler  string      `json:"labeler"`
	Labeling string      `json:"labeling"`
	Queries  []evalQuery `json:"queries"`
}

type evalMetrics struct {
	Queries     int     `json:"queries"`
	Recall1     float64 `json:"recall_at_1"`
	Recall5     float64 `json:"recall_at_5"`
	Recall10    float64 `json:"recall_at_10"`
	MRR         float64 `json:"mrr"`
	MedianBytes int     `json:"median_response_bytes"`
}

type evalBaseline struct {
	Scorer     string                 `json:"scorer"`
	Catalog    string                 `json:"catalog_sha256"`
	Overall    evalMetrics            `json:"overall"`
	ByCategory map[string]evalMetrics `json:"by_category"`
	// Ranks maps query id to the 1-based rank of the first expected tool in the
	// top 50, or 0 for a miss, so a scorer change shows which queries moved.
	Ranks map[string]int `json:"ranks"`
	// NoMatch maps each "nomatch" query id (no tool should match) to how many tools the search returned
	// in its top 50. Fewer is better; zero is a clean refusal. These queries are not part of the recall metrics.
	NoMatch map[string]int `json:"no_match_returned,omitempty"`
}

func (c *evalCatalog) digest() string {
	raw, _ := json.Marshal(c.Entries)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (c *evalCatalog) snapshot() Snapshot {
	snap := Snapshot{}
	seen := map[string]bool{}
	for _, e := range c.Entries {
		schema := map[string]any{"type": "object"}
		if len(e.Required) > 0 {
			schema["required"] = e.Required
		}
		if len(e.Arguments) > 0 {
			props := map[string]any{}
			for _, a := range e.Arguments {
				prop := map[string]any{}
				if a.Description != "" {
					prop["description"] = a.Description
				}
				props[a.Name] = prop
			}
			schema["properties"] = props
		}
		snap.Entries = append(snap.Entries, Entry{
			Tool:   &mcpsdk.Tool{Name: e.Name, Title: e.Title, Description: e.Description, Annotations: e.Annotations, InputSchema: schema},
			Origin: e.Origin,
			Tags:   searchEvalOriginTags[e.Origin],
		})
		if !seen[e.Origin] {
			seen[e.Origin] = true
			snap.Origins = append(snap.Origins, OriginStatus{ID: e.Origin, Status: "connected"})
		}
	}
	return snap
}

func loadEvalCatalog(t testing.TB) *evalCatalog {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(searchEvalDir, "catalog.json"))
	if err != nil {
		t.Fatalf("read catalog snapshot: %v", err)
	}
	var c evalCatalog
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse catalog snapshot: %v", err)
	}
	return &c
}

func loadEvalQueries(t testing.TB) evalQuerySet {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(searchEvalDir, "queries.json"))
	if err != nil {
		t.Fatalf("read queries: %v", err)
	}
	var q evalQuerySet
	if err := json.Unmarshal(raw, &q); err != nil {
		t.Fatalf("parse queries: %v", err)
	}
	return q
}

// runSearchEval scores every query with the scorer and returns the metrics.
// Rank comes from a limit-50 search (the maximum a caller can ask for); the
// response size comes from the default call (summary detail, limit 10), which
// is what an agent actually receives.
func runSearchEval(t testing.TB, cat *evalCatalog, qs []evalQuery, name string, score func(string, Entry) int, scoreAll func(string, []Entry) []int) evalBaseline {
	t.Helper()
	snap := cat.snapshot()
	svc := &Service{Snapshot: func() Snapshot { return snap }, Score: score, ScoreAll: scoreAll}
	type acc struct {
		n, r1, r5, r10 int
		rr             float64
		bytes          []int
	}
	byCat := map[string]*acc{}
	all := &acc{}
	ranks := map[string]int{}
	noMatch := map[string]int{}
	for _, q := range qs {
		if q.Category == "nomatch" {
			res, err := svc.Search(Request{Query: q.Query, Detail: "names", Limit: 50})
			if err != nil {
				t.Fatalf("%s: search %q: %v", q.ID, q.Query, err)
			}
			noMatch[q.ID] = len(res.Items)
			continue
		}
		wanted := map[string]bool{}
		for _, e := range q.Expect {
			wanted[e] = true
		}
		deep, err := svc.Search(Request{Query: q.Query, Detail: "names", Limit: 50})
		if err != nil {
			t.Fatalf("%s: search %q: %v", q.ID, q.Query, err)
		}
		rank := 0
		for i, it := range deep.Items {
			if wanted[it.Name] {
				rank = i + 1
				break
			}
		}
		ranks[q.ID] = rank
		def, err := svc.Search(Request{Query: q.Query})
		if err != nil {
			t.Fatalf("%s: default search %q: %v", q.ID, q.Query, err)
		}
		raw, _ := json.Marshal(def)
		for _, a := range []*acc{all, func() *acc {
			if byCat[q.Category] == nil {
				byCat[q.Category] = &acc{}
			}
			return byCat[q.Category]
		}()} {
			a.n++
			a.bytes = append(a.bytes, len(raw))
			if rank > 0 {
				a.rr += 1 / float64(rank)
				if rank <= 1 {
					a.r1++
				}
				if rank <= 5 {
					a.r5++
				}
				if rank <= 10 {
					a.r10++
				}
			}
		}
	}
	metrics := func(a *acc) evalMetrics {
		sort.Ints(a.bytes)
		n := float64(a.n)
		return evalMetrics{
			Queries: a.n, Recall1: round4(float64(a.r1) / n), Recall5: round4(float64(a.r5) / n),
			Recall10: round4(float64(a.r10) / n), MRR: round4(a.rr / n), MedianBytes: a.bytes[len(a.bytes)/2],
		}
	}
	out := evalBaseline{Scorer: name, Catalog: cat.digest(), Overall: metrics(all), ByCategory: map[string]evalMetrics{}, Ranks: ranks}
	if len(noMatch) > 0 {
		out.NoMatch = noMatch
	}
	for c, a := range byCat {
		out.ByCategory[c] = metrics(a)
	}
	return out
}

func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

func (b evalBaseline) table() string {
	var sb strings.Builder
	row := func(label string, m evalMetrics) {
		fmt.Fprintf(&sb, "  %-8s n=%-4d recall@1=%.3f recall@5=%.3f recall@10=%.3f MRR=%.3f median_bytes=%d\n",
			label, m.Queries, m.Recall1, m.Recall5, m.Recall10, m.MRR, m.MedianBytes)
	}
	fmt.Fprintf(&sb, "scorer %q on catalog %s\n", b.Scorer, b.Catalog[:12])
	row("overall", b.Overall)
	cats := make([]string, 0, len(b.ByCategory))
	for c := range b.ByCategory {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		row(c, b.ByCategory[c])
	}
	if len(b.NoMatch) > 0 {
		total := 0
		for _, n := range b.NoMatch {
			total += n
		}
		fmt.Fprintf(&sb, "  nomatch  n=%-4d returned_any=%d of %d, tools returned in total=%d (lower is better)\n", len(b.NoMatch), noMatchAny(b), len(b.NoMatch), total)
	}
	return sb.String()
}

func baselinePath(name string) string {
	return filepath.Join(searchEvalDir, "baseline_"+name+".json")
}

// TestSearchEval prints the numbers for every registered scorer and fails if
// they differ from the committed baseline. Run with -search-eval-update after
// an intentional scorer or fixture change and commit the new baseline.
func TestSearchEval(t *testing.T) {
	cat := loadEvalCatalog(t)
	qs := loadEvalQueries(t).Queries
	for _, sc := range searchEvalScorers {
		got := runSearchEval(t, cat, qs, sc.Name, sc.Score, sc.ScoreAll)
		t.Log("\n" + got.table())
		if *searchEvalUpdate {
			raw, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(baselinePath(sc.Name), append(raw, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		raw, err := os.ReadFile(baselinePath(sc.Name))
		if err != nil {
			t.Fatalf("no committed baseline for scorer %q (%v); run with -args -search-eval-update", sc.Name, err)
		}
		var want evalBaseline
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatalf("parse baseline: %v", err)
		}
		if want.Catalog != got.Catalog {
			t.Fatalf("baseline %q was recorded on catalog %s but the snapshot is %s; re-record with -args -search-eval-update", sc.Name, want.Catalog[:12], got.Catalog[:12])
		}
		if wj, gj := mustJSON(want), mustJSON(got); wj != gj {
			var moved []string
			for id, r := range got.Ranks {
				if want.Ranks[id] != r {
					moved = append(moved, fmt.Sprintf("%s %d->%d", id, want.Ranks[id], r))
				}
			}
			sort.Strings(moved)
			t.Errorf("scorer %q no longer matches its committed baseline (queries moved: %s)\nbaseline:\n%scurrent:\n%s", sc.Name, strings.Join(moved, ", "), want.table(), got.table())
		}
	}
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

var (
	evalIDLike    = regexp.MustCompile(`(?i)\b(cw|ep|prj|run|msg|agt)[-_][a-z0-9-]{4,}|[0-9a-f]{8}-[0-9a-f]{4}-|\b[0-9a-f]{24,}\b`)
	evalSecret    = regexp.MustCompile(`(?i)(sk-[a-z0-9]{16,}|gh[pousr]_[a-z0-9]{20,}|xox[a-z]-[a-z0-9-]{10,}|bearer\s+[a-z0-9._-]{20,}|(token|secret|password|api[_-]?key)\s*[:=]\s*\S{8,})`)
	evalValueLike = regexp.MustCompile(`[{}"=\\]|https?://`)
)

// TestSearchEvalFixture guards the fixture itself: it must be big enough,
// consistent, pinned, and free of argument values and secrets, and a stale
// label (a tool that is no longer in the snapshot) fails loudly.
func TestSearchEvalFixture(t *testing.T) {
	cat := loadEvalCatalog(t)
	set := loadEvalQueries(t)

	if got := cat.digest(); got != cat.SHA256 {
		t.Fatalf("catalog.json entries do not match their recorded sha256 (recorded %s, computed %s): the snapshot was edited by hand; regenerate it with -args -search-eval-snapshot-from", cat.SHA256, got)
	}
	if cat.Tools != len(cat.Entries) {
		t.Fatalf("catalog.json says %d tools but holds %d", cat.Tools, len(cat.Entries))
	}
	if set.Labeler == "" || set.Labeling == "" {
		t.Fatal("queries.json must say who labeled it and how")
	}
	if len(set.Queries) < 100 {
		t.Fatalf("fixture has %d queries; the minimum is 100", len(set.Queries))
	}

	known := map[string]bool{}
	for _, e := range cat.Entries {
		if known[e.Name] {
			t.Errorf("duplicate tool %q in snapshot", e.Name)
		}
		known[e.Name] = true
		if evalSecret.MatchString(e.Description) {
			t.Errorf("tool %q description looks like it holds a secret", e.Name)
		}
	}

	ids := map[string]bool{}
	cats := map[string]int{}
	origins := map[string]bool{}
	byName := map[string]string{}
	for _, e := range cat.Entries {
		byName[e.Name] = e.Origin
	}
	for _, q := range set.Queries {
		if q.ID == "" || ids[q.ID] {
			t.Errorf("query id %q is empty or duplicated", q.ID)
		}
		ids[q.ID] = true
		cats[q.Category]++
		if q.Category == "nomatch" {
			if strings.TrimSpace(q.Query) == "" || len(q.Expect) != 0 {
				t.Errorf("%s: a nomatch query needs text and no expected tool", q.ID)
			}
		} else if strings.TrimSpace(q.Query) == "" || len(q.Expect) == 0 {
			t.Errorf("%s: needs a query and at least one expected tool", q.ID)
		}
		if evalValueLike.MatchString(q.Query) || evalIDLike.MatchString(q.Query) || evalSecret.MatchString(q.Query) {
			t.Errorf("%s: query %q looks like an argument value, id or secret; queries must be intent only", q.ID, q.Query)
		}
		var stale []string
		for _, name := range q.Expect {
			if !known[name] {
				stale = append(stale, name)
			}
			origins[byName[name]] = true
		}
		if len(stale) > 0 {
			t.Errorf("%s: STALE LABEL: expected tool(s) %v are not in the pinned snapshot; re-snapshot or relabel", q.ID, stale)
		}
	}
	for _, c := range []string{"head", "tail", "hard"} { // "regression" is small by design
		if cats[c] < 20 {
			t.Errorf("category %q has %d queries; want at least 20", c, cats[c])
		}
	}
	if len(origins) < 12 {
		t.Errorf("expected tools span %d origins; want at least 12 so the eval is not one app", len(origins))
	}
	t.Logf("fixture: %d queries %v across %d origins; %d flagged for human review; catalog %d tools sha256 %s",
		len(set.Queries), cats, len(origins), countReview(set.Queries), cat.Tools, cat.SHA256[:12])
}

func countReview(qs []evalQuery) int {
	n := 0
	for _, q := range qs {
		if q.Review {
			n++
		}
	}
	return n
}

// TestSearchEvalSnapshot regenerates catalog.json from a gateway inventory dump
// (the JSON array a detail=schema dump of every tool produces). It only runs
// when -search-eval-snapshot-from is given.
func TestSearchEvalSnapshot(t *testing.T) {
	if *searchEvalSnapshotFrom == "" {
		t.Skip("set -args -search-eval-snapshot-from=<inventory.json> to regenerate catalog.json")
	}
	raw, err := os.ReadFile(*searchEvalSnapshotFrom)
	if err != nil {
		t.Fatal(err)
	}
	var dump []struct {
		Name        string                  `json:"name"`
		Origin      string                  `json:"origin"`
		Title       string                  `json:"title"`
		Description string                  `json:"description"`
		Annotations *mcpsdk.ToolAnnotations `json:"annotations"`
		InputSchema any                     `json:"inputSchema"`
	}
	if err := json.Unmarshal(raw, &dump); err != nil {
		t.Fatal(err)
	}
	cat := evalCatalog{Schema: 1, Captured: os.Getenv("SEARCH_EVAL_CAPTURED"), Source: os.Getenv("SEARCH_EVAL_SOURCE")}
	for _, d := range dump {
		cat.Entries = append(cat.Entries, evalTool{
			Name: d.Name, Origin: d.Origin, Title: d.Title, Description: d.Description,
			Annotations: d.Annotations, Required: requiredArgs(d.InputSchema), Arguments: evalArguments(d.InputSchema),
		})
	}
	sort.Slice(cat.Entries, func(i, j int) bool { return cat.Entries[i].Name < cat.Entries[j].Name })
	cat.Tools = len(cat.Entries)
	cat.SHA256 = cat.digest()

	var sb strings.Builder
	fmt.Fprintf(&sb, "{\n  \"schema\": 1,\n  \"captured\": %q,\n  \"source\": %q,\n  \"tools\": %d,\n  \"sha256\": %q,\n  \"entries\": [\n",
		cat.Captured, cat.Source, cat.Tools, cat.SHA256)
	for i, e := range cat.Entries {
		line, _ := json.Marshal(e)
		sb.WriteString("    " + string(line))
		if i < len(cat.Entries)-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("  ]\n}\n")
	if err := os.WriteFile(filepath.Join(searchEvalDir, "catalog.json"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote catalog.json: %d tools sha256 %s", cat.Tools, cat.SHA256)
}

// TestSearchEvalLiveDrift compares the pinned snapshot with a fresh inventory
// dump and fails when an expected tool has vanished from the live gateway.
// Opt in with SEARCH_EVAL_LIVE_INVENTORY=<inventory.json>.
func TestSearchEvalLiveDrift(t *testing.T) {
	path := os.Getenv("SEARCH_EVAL_LIVE_INVENTORY")
	if path == "" {
		t.Skip("set SEARCH_EVAL_LIVE_INVENTORY=<inventory.json> to check the labels against the live gateway")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var live []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &live); err != nil {
		t.Fatal(err)
	}
	liveSet := map[string]bool{}
	for _, l := range live {
		liveSet[l.Name] = true
	}
	for _, q := range loadEvalQueries(t).Queries {
		for _, name := range q.Expect {
			if !liveSet[name] {
				t.Errorf("%s: expected tool %q is gone from the live gateway; relabel and re-snapshot", q.ID, name)
			}
		}
	}
	pinned := map[string]bool{}
	for _, e := range loadEvalCatalog(t).Entries {
		pinned[e.Name] = true
	}
	added := 0
	for name := range liveSet {
		if !pinned[name] {
			added++
		}
	}
	t.Logf("live gateway has %d tools, snapshot %d; %d live tools are not in the snapshot", len(liveSet), len(pinned), added)
}

// searchEvalRank returns the 1-based position of each named tool in the BM25 ranking of query
// over the pinned catalog (0 when it is not returned).
func searchEvalRanks(t *testing.T, query string, names ...string) map[string]int {
	t.Helper()
	cat := loadEvalCatalog(t)
	entries := cat.snapshot().Entries
	scores := BM25ScoreAll(query, entries)
	type hit struct {
		name  string
		score int
	}
	var hits []hit
	for i, e := range entries {
		if scores[i] > 0 {
			hits = append(hits, hit{e.Tool.Name, scores[i]})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	ranks := map[string]int{}
	for _, n := range names {
		ranks[n] = 0
	}
	for i, h := range hits {
		if _, ok := ranks[h.name]; ok {
			ranks[h.name] = i + 1
		}
	}
	return ranks
}

// CW-20261024-0114: before BM25 the keyword scorer ranked card_show, panel_open and python_run above
// tether_tool_metrics for this query, because they matched generic words. They must stay below it.
func TestSearchRegressionTelemetryQuery(t *testing.T) {
	const query = "tool call telemetry proxy events stats error class latency"
	r := searchEvalRanks(t, query, "tether_tool_metrics", "card_show", "panel_open", "python_run")
	if r["tether_tool_metrics"] == 0 || r["tether_tool_metrics"] > 5 {
		t.Fatalf("tether_tool_metrics rank = %d, want in the top 5", r["tether_tool_metrics"])
	}
	for _, noise := range []string{"card_show", "panel_open", "python_run"} {
		if r[noise] != 0 && r[noise] < r["tether_tool_metrics"] {
			t.Errorf("%s (rank %d) outranks tether_tool_metrics (rank %d)", noise, r[noise], r["tether_tool_metrics"])
		}
	}
}

// The N2b gate: no regression on exact names and name prefixes. An exact name is first; a unique prefix
// of a tool's name finds that tool in the top 3.
func TestSearchRegressionExactNameAndPrefix(t *testing.T) {
	for _, name := range []string{"torque_task_get", "tether_message_send", "cerberus_resource_status", "tether_catalog_plan", "tesseract_recall", "tether_session_health"} {
		if got := searchEvalRanks(t, name, name)[name]; got != 1 {
			t.Errorf("exact name %q ranks %d, want 1", name, got)
		}
	}
	for prefix, want := range map[string]string{"tether_session_hea": "tether_session_health", "tether_catalog_pl": "tether_catalog_plan", "torque_comment_ad": "torque_comment_add"} {
		if got := searchEvalRanks(t, prefix, want)[want]; got == 0 || got > 3 {
			t.Errorf("prefix %q finds %q at rank %d, want top 3", prefix, want, got)
		}
	}
}

// evalArguments lists a schema's top-level properties with a trimmed description.
func evalArguments(schema any) []evalArgument {
	m, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	props, _ := m["properties"].(map[string]any)
	var out []evalArgument
	for name, raw := range props {
		a := evalArgument{Name: name}
		if prop, ok := raw.(map[string]any); ok {
			if d, ok := prop["description"].(string); ok {
				a.Description = truncateRunes(d, 160)
			}
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TestSearchEvalVariants compares index variants on the same fixture (CW-20261024-0115). It only reports;
// the shipped default is whatever bm25Variant{} does, and a change is kept only when this table shows
// recall@5 and MRR improve with the exact-name and prefix cases (category "regression") not worse.
func TestSearchEvalVariants(t *testing.T) {
	if os.Getenv("SEARCH_EVAL_VARIANTS") == "" {
		t.Skip("set SEARCH_EVAL_VARIANTS=1 to compare index variants (about 45s)")
	}
	cat := loadEvalCatalog(t)
	qs := loadEvalQueries(t).Queries
	variants := []struct {
		name string
		v    bm25Variant
	}{
		{"baseline", bm25Variant{}},
		{"args:names", bm25Variant{ArgNames: true}},
		{"args:names+desc", bm25Variant{ArgDescriptions: true}},
		{"stem", bm25Variant{Stem: true}},
		{"name-boost x1", bm25Variant{NameBoost: 1}},
		{"name-boost x2", bm25Variant{NameBoost: 2}},
		{"name-boost x3", bm25Variant{NameBoost: 3}},
		{"name-boost x4", bm25Variant{NameBoost: 4}},
		{"name-boost x2+args", bm25Variant{NameBoost: 2, ArgNames: true}},
		{"name-boost x3+stopwords", bm25Variant{NameBoost: 3, ExtraStopwords: true}},
		{"args:names+stem", bm25Variant{ArgNames: true, Stem: true}},
		{"args:names+desc+stem", bm25Variant{ArgDescriptions: true, Stem: true}},
	}
	shipped := searchVariant
	defer func() { searchVariant = shipped }()
	var sb strings.Builder
	for _, tc := range variants {
		searchVariant = tc.v
		b := runSearchEval(t, cat, qs, tc.name, nil, BM25ScoreAll)
		fmt.Fprintf(&sb, "%-22s recall@1=%.3f recall@5=%.3f recall@10=%.3f MRR=%.3f  hard@5=%.3f regression@1=%.3f nomatch_any=%d/%d | heldout r@1=%.3f r@5=%.3f MRR=%.3f | x009=%d x011=%d\n",
			tc.name, b.Overall.Recall1, b.Overall.Recall5, b.Overall.Recall10, b.Overall.MRR, b.ByCategory["hard"].Recall5, b.ByCategory["regression"].Recall1, noMatchAny(b), len(b.NoMatch),
			b.ByCategory["heldout"].Recall1, b.ByCategory["heldout"].Recall5, b.ByCategory["heldout"].MRR, b.Ranks["x009"], b.Ranks["x011"])
	}
	t.Logf("\n%s", sb.String())
}

func noMatchAny(b evalBaseline) int {
	n := 0
	for _, r := range b.NoMatch {
		if r > 0 {
			n++
		}
	}
	return n
}
