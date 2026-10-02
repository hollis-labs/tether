package mcpgateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"sort"
)

// ProtocolList uses the same eligible snapshot and ordering as semantic list;
// only the mode's infrastructure is exempt from target filtering. Registered
// definitions are provided by the transport after reading all SDK pages.
func (s *Service) ProtocolList(registered []*mcpsdk.Tool, infrastructure func(string) bool, cursor string) ([]*mcpsdk.Tool, string, error) {
	policy := s.Policy
	if policy == nil {
		policy = &Policy{}
	}
	snapshot := policy.Eligible(s.Snapshot())
	unavailable, _ := availability(snapshot)
	eligible := map[string]Entry{}
	for _, entry := range snapshot.Entries {
		if !unavailable[entry.Origin] {
			eligible[entry.Tool.Name] = entry
		}
	}
	entries := []Entry{}
	for _, tool := range registered {
		if infrastructure(tool.Name) {
			entries = append(entries, policy.Decorate(Entry{Tool: tool, Origin: "tether"}))
			continue
		}
		if s.Selection.Mode == Flat {
			if entry, ok := eligible[tool.Name]; ok {
				entries = append(entries, entry)
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return policy.Less(entries[i], entries[j]) })
	raw, _ := json.Marshal(struct {
		Selection Selection
		Policy    *Policy
		Entries   []Entry
	}{s.Selection, s.Policy, entries})
	binding := fmt.Sprintf("%x", sha256.Sum256(raw))
	offset, err := readCursor(cursor, binding, len(entries))
	if err != nil {
		return nil, "", err
	}
	end := min(offset+mcpsdk.DefaultPageSize, len(entries))
	tools := []*mcpsdk.Tool{}
	next := ""
	for _, entry := range entries[offset:end] {
		tools = append(tools, entry.Tool)
	}
	if end < len(entries) {
		raw, _ := json.Marshal(pageCursor{binding, end})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	return tools, next, nil
}
