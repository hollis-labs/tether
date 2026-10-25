package mcpadapter

import (
	"context"
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func rawToolsList(t *testing.T, cs *mcpsdk.ClientSession) (*mcpsdk.ListToolsResult, []byte) {
	t.Helper()
	res, err := cs.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	b, err := json.Marshal(res.Tools)
	if err != nil {
		t.Fatal(err)
	}
	return res, b
}

// CW-20261024-0054: the search-mode front is byte-identical across a restart (each
// connectProxyMode starts a fresh process) and across a change in which upstreams exist.
func TestSearchModeToolsListIsByteStable(t *testing.T) {
	catalog, _ := proxyCatalog(t, "alpha")
	_, first := rawToolsList(t, connectProxyMode(t, catalog, "search", false, "alpha"))
	_, restarted := rawToolsList(t, connectProxyMode(t, catalog, "search", false, "alpha"))
	if string(first) != string(restarted) {
		t.Fatalf("tools/list changed across a restart:\n%s\n%s", first, restarted)
	}
	more, _ := proxyCatalog(t, "alpha", "beta", "gamma")
	_, changed := rawToolsList(t, connectProxyMode(t, more, "search", false, "alpha", "beta", "gamma"))
	if string(first) != string(changed) {
		t.Fatalf("tools/list changed when upstreams changed:\n%s\n%s", first, changed)
	}
}

func TestToolsListCarriesCacheHints(t *testing.T) {
	catalog, _ := proxyCatalog(t, "alpha")
	for _, tc := range []struct {
		mode string
		ttl  int
	}{{"search", searchListTTLMs}, {"flat", flatListTTLMs}} {
		res, _ := rawToolsList(t, connectProxyMode(t, catalog, tc.mode, false, "alpha"))
		if res.TTLMs != tc.ttl || res.CacheScope != "private" {
			t.Errorf("%s: ttlMs=%d cacheScope=%q, want %d private", tc.mode, res.TTLMs, res.CacheScope, tc.ttl)
		}
	}
}
