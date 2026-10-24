package mcpgateway

// Result cap (CW-20261024-0052, implements CW-20261001-0554).
//
// Some tools return hundreds of kilobytes in one result (a catalog listing, a
// registry search, an event history). Clients then spill the output to a file
// or truncate it blindly, which wastes tokens and loses the result. This file
// is the gateway's safety net: a read result over the cap is cut at an item
// boundary of its largest array, still valid JSON, with a marker and a cursor
// that resumes from a server-side copy of the original. The fix for any one
// tool is owner-side paging (CW-20261024-0053); this catches the rest.
//
// Rules:
//   - Only successful results of tools annotated read-only are cut. An error is
//     never cut (the next step is in its text) and neither is a write receipt
//     (the caller must see exactly what happened).
//   - A result with no array to cut, or whose text is not JSON, is delivered
//     whole and reported as skipped; the cap never invents a cut point.
//   - The cursor serves slices of one stored snapshot, so pages cannot gap or
//     repeat even when the data changes between calls.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultResultCapBytes is the starting cap on the encoded size of one tool
// result. It is a starting value, to be confirmed against measured p50/p95.
const DefaultResultCapBytes = 32 << 10

// ResultCapEnv overrides the default cap in bytes. 0 or a negative value turns
// the cap off, which is the rollback switch.
const ResultCapEnv = "TETHER_MCP_RESULT_CAP_BYTES"

// ResultNextTool is the native tool that serves the next slice of a capped result.
const ResultNextTool = "tether_result_next"

const (
	maxStashBytes   = 8 << 20
	maxStashTotal   = 48 << 20
	maxStashEntries = 64
	stashTTL        = 10 * time.Minute
	maxArrayDepth   = 6
)

// ResultCap is the optional per-profile override. Bytes: 0 inherits the default,
// a positive value sets the cap, -1 removes it. Tools overrides by exact tool name.
type ResultCap struct {
	DefaultBytes int            `yaml:"default_bytes" json:"default_bytes,omitempty"`
	Tools        map[string]int `yaml:"tools" json:"tools,omitempty"`
}

// Validate rejects values that mean nothing.
func (c *ResultCap) Validate() error {
	if c == nil {
		return nil
	}
	check := func(label string, n int) error {
		if n < -1 || (n > 0 && n < 1024) {
			return fmt.Errorf("%s: %d is not -1 (unlimited), 0 (inherit) or at least 1024 bytes", label, n)
		}
		return nil
	}
	if err := check("result_cap.default_bytes", c.DefaultBytes); err != nil {
		return err
	}
	for name, n := range c.Tools {
		if name == "" {
			return fmt.Errorf("result_cap.tools: empty tool name")
		}
		if err := check("result_cap.tools."+name, n); err != nil {
			return err
		}
	}
	return nil
}

// For returns the effective cap in bytes for a tool: base is the daemon default
// (0 or less means no cap), then the profile default, then the per-tool value.
// A result of 0 or less means unlimited.
func (c *ResultCap) For(tool string, base int) int {
	limit := base
	if c != nil {
		if c.DefaultBytes != 0 {
			limit = c.DefaultBytes
		}
		if n, ok := c.Tools[tool]; ok && n != 0 {
			limit = n
		}
	}
	if limit < 0 {
		return 0
	}
	return limit
}

// CapReport says what the cap did to one result, for telemetry.
type CapReport struct {
	OriginalBytes  int
	DeliveredBytes int
	Truncated      bool
	// Skipped is why an oversize result was delivered whole ("no_array",
	// "not_json"), or cut without a cursor ("stash_full", "too_large_for_cursor").
	Skipped  string // see Cap; "stash_full" and "too_large_for_cursor" were still cut, without a cursor
	Path     string
	Returned int
	Total    int
}

type stash struct {
	tool    string
	tree    any
	path    []string
	total   int
	size    int
	expires time.Time
}

// ResultPager cuts oversize results and serves the rest by cursor. It holds the
// original results in memory only, for ten minutes, in bounded space, and is
// safe for concurrent use. A pager belongs to one principal's view, so a cursor
// is only ever redeemed by the caller that could have made the original call.
type ResultPager struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*stash
	total   int
}

// NewResultPager returns an empty pager.
func NewResultPager() *ResultPager {
	return &ResultPager{now: time.Now, entries: map[string]*stash{}}
}

func jsonCompact(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

func resultSize(res *mcpsdk.CallToolResult) int {
	raw, err := json.Marshal(res)
	if err != nil {
		return 0
	}
	return len(raw)
}

func decodeJSON(s string) (any, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	return v, true
}

// bigArray is the largest array in a tree: the path of object keys to it.
type bigArray struct {
	path []string
	size int
	n    int
}

func findBigArray(v any, path []string, best *bigArray, depth int) {
	switch t := v.(type) {
	case []any:
		if len(t) >= 2 {
			if size := len(jsonCompact(t)); size > best.size {
				best.path, best.size, best.n = append([]string(nil), path...), size, len(t)
			}
		}
		// Arrays of arrays are cut at the outer level; do not descend.
	case map[string]any:
		if depth >= maxArrayDepth {
			return
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			findBigArray(t[k], append(path, k), best, depth+1)
		}
	}
}

// at returns the array at path, which may be the root (empty path).
func at(v any, path []string) ([]any, bool) {
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		v = m[key]
	}
	arr, ok := v.([]any)
	return arr, ok
}

// withSlice returns a copy of tree whose array at path is items. Only the maps on
// the way to the array are copied; everything else is shared and not mutated.
func withSlice(tree any, path []string, items []any) any {
	if len(path) == 0 {
		return items
	}
	m, ok := tree.(map[string]any)
	if !ok {
		return tree
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	out[path[0]] = withSlice(m[path[0]], path[1:], items)
	return out
}

func pathString(path []string) string {
	if len(path) == 0 {
		return "$"
	}
	return strings.Join(path, ".")
}

// firstJSONText returns the index of the first text block that holds a JSON
// object or array, and its parsed value.
func firstJSONText(res *mcpsdk.CallToolResult) (int, any, bool) {
	for i, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			if v, ok := decodeJSON(tc.Text); ok {
				return i, v, true
			}
		}
	}
	return 0, nil, false
}

type pageMarker struct {
	Truncated     bool   `json:"truncated"`
	Path          string `json:"path"`
	From          int    `json:"from"`
	Returned      int    `json:"returned"`
	Total         int    `json:"total"`
	OriginalBytes int    `json:"original_bytes,omitempty"`
	Cursor        string `json:"next_cursor,omitempty"`
	HowToContinue string `json:"how_to_continue,omitempty"`
}

// render builds the result for items [from, from+k) of the stashed snapshot.
func render(base *mcpsdk.CallToolResult, textIdx int, tree any, structured any, path []string, items []any, from, k int, marker pageMarker) *mcpsdk.CallToolResult {
	slice := items[from : from+k]
	out := *base
	out.Content = append([]mcpsdk.Content(nil), base.Content...)
	out.Content[textIdx] = &mcpsdk.TextContent{Text: jsonCompact(withSlice(tree, path, slice))}
	if structured != nil {
		if sitems, ok := at(structured, path); ok && len(sitems) == len(items) {
			out.StructuredContent = withSlice(structured, path, sitems[from:from+k])
		} else {
			out.StructuredContent = nil
		}
	}
	marker.From, marker.Returned = from, k
	out.Content = append(out.Content, &mcpsdk.TextContent{Text: jsonCompact(struct {
		Marker pageMarker `json:"gateway_truncated"`
	}{marker})})
	return &out
}

// fit returns the largest k in [1, len(items)-from] whose rendering is within
// limit, or 1 when even one item does not fit (the first item is always sent).
func fit(limit int, build func(k int) *mcpsdk.CallToolResult, most int) (int, *mcpsdk.CallToolResult) {
	lo, hi := 1, most
	best := build(1)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if r := build(mid); resultSize(r) <= limit {
			lo, best = mid, r
		} else {
			hi = mid - 1
		}
	}
	return lo, best
}

func newCursorID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Cap enforces limit on res for a tool. readOnly says whether the tool is
// annotated read-only; only then is a result cut. limit <= 0 disables the cap.
// The returned result is a copy when it was cut; res is never mutated.
func (p *ResultPager) Cap(tool string, readOnly bool, res *mcpsdk.CallToolResult, limit int) (*mcpsdk.CallToolResult, CapReport) {
	if res == nil || limit <= 0 {
		return res, CapReport{}
	}
	size := resultSize(res)
	report := CapReport{OriginalBytes: size, DeliveredBytes: size}
	if size <= limit || res.IsError || !readOnly {
		return res, report
	}
	textIdx, tree, ok := firstJSONText(res)
	if !ok {
		report.Skipped = "not_json"
		return res, report
	}
	var best bigArray
	findBigArray(tree, nil, &best, 0)
	if best.n < 2 {
		report.Skipped = "no_array"
		return res, report
	}
	items, _ := at(tree, best.path)
	var structured any
	if res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			if v, ok := decodeJSON(string(raw)); ok {
				structured = v
			}
		}
	}
	marker := pageMarker{Truncated: true, Path: pathString(best.path), Total: len(items), OriginalBytes: size}
	build := func(k int) *mcpsdk.CallToolResult {
		m := marker
		m.Cursor = strings.Repeat("0", 32) + "." + strconv.Itoa(k) // same width as a real cursor
		m.HowToContinue = continueText(m.Cursor)
		return render(res, textIdx, tree, structured, best.path, items, 0, k, m)
	}
	k, _ := fit(limit, build, len(items)-1)
	// Re-render with the real cursor now that k is known.
	cursor := ""
	if size <= maxStashBytes {
		if id, err := newCursorID(); err == nil && p.put(id, &stash{tool: tool, tree: tree, path: best.path, total: len(items), size: size}) {
			cursor = id + "." + strconv.Itoa(k)
		}
	}
	if cursor == "" {
		marker.HowToContinue = "the rest of this result is not available by cursor; call the tool again with narrower arguments (a filter or a smaller limit)"
	} else {
		marker.Cursor, marker.HowToContinue = cursor, continueText(cursor)
	}
	out := render(res, textIdx, tree, structured, best.path, items, 0, k, marker)
	switch {
	case cursor == "" && size <= maxStashBytes:
		report.Skipped = "stash_full"
	case cursor == "":
		report.Skipped = "too_large_for_cursor"
	}
	report.Truncated, report.DeliveredBytes = true, resultSize(out)
	report.Path, report.Returned, report.Total = marker.Path, k, len(items)
	return out, report
}

func continueText(cursor string) string {
	return fmt.Sprintf("call %s with {\"cursor\":%q}; in search mode dispatch it through tether_tool_call", ResultNextTool, cursor)
}

func (p *ResultPager) put(id string, s *stash) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	for len(p.entries) >= maxStashEntries || p.total+s.size > maxStashTotal {
		if !p.evictOldestLocked() {
			return false
		}
	}
	s.expires = p.now().Add(stashTTL)
	p.entries[id] = s
	p.total += s.size
	return true
}

func (p *ResultPager) sweepLocked() {
	now := p.now()
	for id, s := range p.entries {
		if now.After(s.expires) {
			p.total -= s.size
			delete(p.entries, id)
		}
	}
}

func (p *ResultPager) evictOldestLocked() bool {
	var oldest string
	var at time.Time
	for id, s := range p.entries {
		if oldest == "" || s.expires.Before(at) {
			oldest, at = id, s.expires
		}
	}
	if oldest == "" {
		return false
	}
	p.total -= p.entries[oldest].size
	delete(p.entries, oldest)
	return true
}

// ErrUnknownCursor is returned for a cursor that is malformed, expired,
// evicted or never issued; the cases are deliberately not distinguished.
var ErrUnknownCursor = fmt.Errorf("unknown or expired cursor: call the tool again to get a fresh result")

// Next serves the slice that starts at the cursor's offset, cut to limit. The
// same cursor always returns the same slice. limit <= 0 means the default.
func (p *ResultPager) Next(cursor string, limit int) (*mcpsdk.CallToolResult, CapReport, error) {
	id, offText, ok := strings.Cut(cursor, ".")
	if !ok {
		return nil, CapReport{}, ErrUnknownCursor
	}
	from, err := strconv.Atoi(offText)
	if err != nil || from < 0 {
		return nil, CapReport{}, ErrUnknownCursor
	}
	p.mu.Lock()
	p.sweepLocked()
	s := p.entries[id]
	if s != nil {
		s.expires = p.now().Add(stashTTL)
	}
	p.mu.Unlock()
	if s == nil {
		return nil, CapReport{}, ErrUnknownCursor
	}
	items, _ := at(s.tree, s.path)
	if from >= len(items) {
		return nil, CapReport{}, ErrUnknownCursor
	}
	if limit <= 0 {
		limit = DefaultResultCapBytes
	}
	base := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{}}}
	marker := pageMarker{Truncated: true, Path: pathString(s.path), Total: len(items)}
	build := func(k int) *mcpsdk.CallToolResult {
		m := marker
		if from+k < len(items) {
			m.Cursor = id + "." + strconv.Itoa(from+k)
			m.HowToContinue = continueText(m.Cursor)
		}
		m.Truncated = from+k < len(items)
		return render(base, 0, s.tree, nil, s.path, items, from, k, m)
	}
	k, out := fit(limit, build, len(items)-from)
	// The snapshot stays until its TTL even after the last page, so repeating a
	// cursor (a retry) returns the same slice instead of an error.
	rep := CapReport{DeliveredBytes: resultSize(out), Truncated: from+k < len(items), Path: marker.Path, Returned: k, Total: len(items)}
	return out, rep, nil
}

// Entries reports how many snapshots are held, for tests and health output.
func (p *ResultPager) Entries() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}
