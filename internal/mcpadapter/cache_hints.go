package mcpadapter

import "github.com/hollis-labs/tether/internal/mcpgateway"

// Cache hints for tools/list (CW-20261024-0054). The SDK would send ttlMs 0 (stale at
// once) and cacheScope "public". Both are wrong for us: the list a caller gets depends on
// its profile and credentials, so no shared intermediary may reuse it ("private"); and
// the search-mode front is four fixed tools that change only with the binary, so a
// client that re-fetches every few seconds is wasting calls and invites provider
// prompt-cache churn.
//
// A hint never replaces list_changed: clients must still treat a cached list as stale on
// notifications/tools/list_changed, and the gateway sends that when an upstream's tools
// change.
const (
	// searchListTTLMs is for the fixed search-mode front.
	searchListTTLMs = 300_000
	// flatListTTLMs is for flat mode, whose list follows the upstreams and so changes more
	// often; list_changed covers the changes, the TTL only bounds a missed notification.
	flatListTTLMs = 30_000
)

// listCacheScope is "private" in every mode: the list depends on the caller's profile and
// credentials, so a shared intermediary must not reuse it.
const listCacheScope = "private"

// listCacheTTLMs returns the ttlMs for a tools/list result in mode.
func listCacheTTLMs(mode mcpgateway.Mode) int {
	if mode == mcpgateway.Search {
		return searchListTTLMs
	}
	return flatListTTLMs
}
