package mcpgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func bigResult(n int, wrap string) *mcpsdk.CallToolResult {
	items := make([]map[string]any, n)
	for i := range items {
		items[i] = map[string]any{"id": i, "name": fmt.Sprintf("item-%04d", i), "note": strings.Repeat("x", 200), "big": int64(9007199254740993) + int64(i)}
	}
	var body any = items
	switch wrap {
	case "data.items":
		body = map[string]any{"ok": true, "data": map[string]any{"items": items, "meta": "kept"}}
	case "data":
		body = map[string]any{"ok": true, "data": items}
	}
	raw, _ := json.Marshal(body)
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(raw)}}}
}

func idsIn(t *testing.T, res *mcpsdk.CallToolResult, path ...string) []int {
	t.Helper()
	var tree any
	if err := json.Unmarshal([]byte(res.Content[0].(*mcpsdk.TextContent).Text), &tree); err != nil {
		t.Fatalf("result text is not valid JSON: %v", err)
	}
	arr, ok := at(tree, path)
	if !ok {
		t.Fatalf("no array at %v", path)
	}
	out := make([]int, len(arr))
	for i, it := range arr {
		out[i] = int(it.(map[string]any)["id"].(float64))
	}
	return out
}

func markerOf(t *testing.T, res *mcpsdk.CallToolResult) pageMarker {
	t.Helper()
	last := res.Content[len(res.Content)-1].(*mcpsdk.TextContent).Text
	var wrap struct {
		M pageMarker `json:"gateway_truncated"`
	}
	if err := json.Unmarshal([]byte(last), &wrap); err != nil {
		t.Fatalf("marker block is not JSON: %v (%s)", err, last)
	}
	return wrap.M
}

func TestCapLeavesSmallErrorAndWriteResultsAlone(t *testing.T) {
	p := NewResultPager()
	small := bigResult(3, "data")
	if got, rep := p.Cap("t", true, small, 32<<10); got != small || rep.Truncated {
		t.Fatal("a result under the cap must be returned untouched")
	}
	big := bigResult(500, "data")
	if got, rep := p.Cap("t", false, big, 4<<10); got != big || rep.Truncated {
		t.Fatal("a result of a tool not annotated read-only (a write receipt) must be delivered whole")
	}
	errRes := bigResult(500, "data")
	errRes.IsError = true
	if got, rep := p.Cap("t", true, errRes, 4<<10); got != errRes || rep.Truncated {
		t.Fatal("an error result must never be cut")
	}
	if got, rep := p.Cap("t", true, big, 0); got != big || rep.OriginalBytes != 0 {
		t.Fatal("limit 0 disables the cap")
	}
}

func TestCapCutsAtItemBoundaryWithValidJSONAndResumesWithoutGapsOrRepeats(t *testing.T) {
	for _, tc := range []struct {
		wrap string
		path []string
	}{{"root", nil}, {"data", []string{"data"}}, {"data.items", []string{"data", "items"}}} {
		t.Run(tc.wrap, func(t *testing.T) {
			p := NewResultPager()
			const limit = 6 << 10
			res := bigResult(400, tc.wrap)
			out, rep := p.Cap("big_list", true, res, limit)
			if !rep.Truncated || rep.Total != 400 || rep.Returned < 1 || rep.Returned >= 400 {
				t.Fatalf("report=%+v", rep)
			}
			if rep.DeliveredBytes > limit {
				t.Fatalf("delivered %d bytes, cap %d", rep.DeliveredBytes, limit)
			}
			if rep.OriginalBytes <= limit {
				t.Fatalf("original %d should exceed the cap", rep.OriginalBytes)
			}
			got := idsIn(t, out, tc.path...)
			m := markerOf(t, out)
			if !m.Truncated || m.Cursor == "" || m.Total != 400 || m.Returned != len(got) || m.From != 0 || !strings.Contains(m.HowToContinue, ResultNextTool) {
				t.Fatalf("marker=%+v", m)
			}
			if tc.wrap == "data.items" && !strings.Contains(out.Content[0].(*mcpsdk.TextContent).Text, `"meta":"kept"`) {
				t.Fatal("fields next to the cut array must be kept")
			}
			if !strings.Contains(out.Content[0].(*mcpsdk.TextContent).Text, "9007199254740993") {
				t.Fatal("large integers must survive the cut unchanged")
			}
			// Walk every page: the union is exactly 0..399, once each, in order.
			cursor := m.Cursor
			for pages := 0; cursor != "" && pages < 1000; pages++ {
				page, prep, err := p.Next(cursor, limit)
				if err != nil {
					t.Fatalf("next: %v", err)
				}
				if prep.DeliveredBytes > limit {
					t.Fatalf("page of %d bytes exceeds cap %d", prep.DeliveredBytes, limit)
				}
				got = append(got, idsIn(t, page, tc.path...)...)
				cursor = markerOf(t, page).Cursor
			}
			if len(got) != 400 {
				t.Fatalf("collected %d items, want 400", len(got))
			}
			for i, id := range got {
				if id != i {
					t.Fatalf("item %d is id %d: gap or repeat", i, id)
				}
			}
		})
	}
}

func TestCapAlwaysSendsAtLeastTheFirstItem(t *testing.T) {
	p := NewResultPager()
	out, rep := p.Cap("t", true, bigResult(5, "data"), 1)
	if !rep.Truncated || rep.Returned != 1 {
		t.Fatalf("report=%+v", rep)
	}
	if ids := idsIn(t, out, "data"); len(ids) != 1 || ids[0] != 0 {
		t.Fatalf("ids=%v", ids)
	}
}

func TestCapTrimsStructuredContentTheSameWay(t *testing.T) {
	p := NewResultPager()
	res := bigResult(300, "data")
	var tree any
	_ = json.Unmarshal([]byte(res.Content[0].(*mcpsdk.TextContent).Text), &tree)
	res.StructuredContent = tree
	out, rep := p.Cap("t", true, res, 8<<10)
	if !rep.Truncated || rep.DeliveredBytes > 8<<10 {
		t.Fatalf("report=%+v", rep)
	}
	sc := out.StructuredContent.(map[string]any)["data"].([]any)
	if len(sc) != len(idsIn(t, out, "data")) {
		t.Fatalf("structured has %d items, text has %d", len(sc), len(idsIn(t, out, "data")))
	}
}

func TestCapDeliversWholeWhenThereIsNothingToCut(t *testing.T) {
	p := NewResultPager()
	text := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: strings.Repeat("plain text ", 3000)}}}
	if got, rep := p.Cap("t", true, text, 4<<10); got != text || rep.Skipped != "not_json" {
		t.Fatalf("report=%+v", rep)
	}
	obj := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"blob":"` + strings.Repeat("y", 20000) + `"}`}}}
	if got, rep := p.Cap("t", true, obj, 4<<10); got != obj || rep.Skipped != "no_array" {
		t.Fatalf("report=%+v", rep)
	}
}

func TestCursorIsOpaqueExpiringAndBounded(t *testing.T) {
	p := NewResultPager()
	now := time.Now()
	p.now = func() time.Time { return now }
	out, _ := p.Cap("t", true, bigResult(300, "data"), 6<<10)
	cursor := markerOf(t, out).Cursor
	if _, _, err := p.Next(cursor, 6<<10); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "nodot", "deadbeef.5", strings.Split(cursor, ".")[0] + ".-1", strings.Split(cursor, ".")[0] + ".999999", strings.Split(cursor, ".")[0] + ".x"} {
		if _, _, err := p.Next(bad, 0); !errors.Is(err, ErrUnknownCursor) {
			t.Fatalf("cursor %q: err=%v, want ErrUnknownCursor", bad, err)
		}
	}
	a, _, _ := p.Next(cursor, 6<<10)
	b, _, _ := p.Next(cursor, 6<<10)
	if a.Content[0].(*mcpsdk.TextContent).Text != b.Content[0].(*mcpsdk.TextContent).Text {
		t.Fatal("the same cursor must return the same page")
	}
	now = now.Add(stashTTL + time.Minute)
	if _, _, err := p.Next(cursor, 0); !errors.Is(err, ErrUnknownCursor) {
		t.Fatalf("expired cursor: err=%v", err)
	}
	if p.Entries() != 0 {
		t.Fatalf("expired snapshot still held: %d", p.Entries())
	}
	for i := 0; i < maxStashEntries+10; i++ {
		p.Cap("t", true, bigResult(50, "data"), 4<<10)
	}
	if p.Entries() > maxStashEntries {
		t.Fatalf("held %d snapshots, bound is %d", p.Entries(), maxStashEntries)
	}
}

func TestResultCapConfig(t *testing.T) {
	var none *ResultCap
	if none.For("x", 100) != 100 || none.For("x", 0) != 0 || none.For("x", -1) != 0 {
		t.Fatal("nil config inherits the base; a base of 0 or less is unlimited")
	}
	c := &ResultCap{DefaultBytes: 2048, Tools: map[string]int{"huge": -1, "tight": 1024}}
	for name, want := range map[string]int{"other": 2048, "huge": 0, "tight": 1024} {
		if got := c.For(name, 32768); got != want {
			t.Errorf("For(%q)=%d want %d", name, got, want)
		}
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*ResultCap{{DefaultBytes: 10}, {DefaultBytes: -2}, {Tools: map[string]int{"": 2048}}, {Tools: map[string]int{"x": 5}}} {
		if bad.Validate() == nil {
			t.Errorf("%+v should be invalid", bad)
		}
	}
}
