package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/callcontext"
)

// Search describes one tether_tool_search result for telemetry. Names are the
// returned tools in rank order; they feed the search->call linker and are never
// stored on the event. Terms are the normalized query words (CW-20261024-0048).
type Search struct {
	Query     string
	Results   int
	Detail    string
	Truncated bool
	Names     []string
}

const (
	maxSearchTerms   = 12
	maxSearchTermLen = 32
	// A call is linked to a search whose results contained it for this long, and
	// to the latest search regardless of rank for the shorter window, so an agent
	// that simply knows the tool name is not counted as having used the search.
	linkContainedWindow = 15 * time.Minute
	linkLatestWindow    = 2 * time.Minute
	linkerMaxKeys       = 2048
	linkerPerKey        = 3
	linkerNames         = 50
)

// NormalizeTerms lowercases q and splits it into at most maxSearchTerms unique
// alphanumeric words of 2..maxSearchTermLen characters, in first-seen order.
func NormalizeTerms(q string) []string {
	isAlnum := func(r rune) bool { return 'a' <= r && r <= 'z' || '0' <= r && r <= '9' }
	var out []string
	seen := map[string]struct{}{}
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return !isAlnum(r) }) {
		if len(w) < 2 {
			continue
		}
		if len(w) > maxSearchTermLen {
			w = w[:maxSearchTermLen]
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		out = append(out, w)
		if len(out) == maxSearchTerms {
			break
		}
	}
	return out
}

// IsDiscoveryTool reports whether name is one of the gateway's own discovery
// tools, which are not themselves "calls that followed a search".
func IsDiscoveryTool(name string) bool {
	switch name {
	case "tether_tool_search", "tether_tool_list", "tether_tool_call", "tether_gateway_status":
		return true
	}
	return false
}

// SetSearch records search details on the current call's observation and
// registers its result names with the linker so later calls can be attributed.
func SetSearch(ctx context.Context, s Search) {
	o, ok := ctx.Value(observationKey{}).(*Observation)
	if !ok {
		return
	}
	id := newSearchID()
	o.mu.Lock()
	o.event.SearchID = id
	o.event.SearchTerms = strings.Join(NormalizeTerms(s.Query), " ")
	o.event.SearchResults = s.Results
	o.event.SearchZero = s.Results == 0
	o.event.SearchDetail = s.Detail
	o.event.SearchTruncated = s.Truncated
	key := linkKeyFor(ctx, o.event.Attribution.PrincipalID)
	o.mu.Unlock()
	searches.add(key, id, s.Names, time.Now())
}

// SetResolved records the downstream tool a tether_tool_call dispatch targets.
func SetResolved(ctx context.Context, name string) {
	if o, ok := ctx.Value(observationKey{}).(*Observation); ok {
		o.mu.Lock()
		o.event.ResolvedTool = boundedToolName(name)
		o.mu.Unlock()
	}
}

func newSearchID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// linkKeyFor groups calls by connection. The transport session hash is the
// precise key; without one (stdio, in-process) fall back to the claimed label
// and principal, which is coarser and may merge concurrent clients.
func linkKeyFor(ctx context.Context, principal string) string {
	if h := callcontext.MCPSession(ctx); h != "" {
		return "s:" + h
	}
	return "p:" + principal + "|" + callcontext.ClientLabel(ctx)
}

type searchRecord struct {
	id    string
	at    time.Time
	ranks map[string]int
}

type searchLinker struct {
	mu   sync.Mutex
	keys map[string][]searchRecord
}

var searches = &searchLinker{keys: map[string][]searchRecord{}}

func (l *searchLinker) add(key, id string, names []string, now time.Time) {
	if key == "" || id == "" {
		return
	}
	if len(names) > linkerNames {
		names = names[:linkerNames]
	}
	ranks := make(map[string]int, len(names))
	for i, n := range names {
		if _, dup := ranks[n]; !dup {
			ranks[n] = i + 1
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	recs := append(l.keys[key], searchRecord{id: id, at: now, ranks: ranks})
	if len(recs) > linkerPerKey {
		recs = recs[len(recs)-linkerPerKey:]
	}
	l.keys[key] = recs
	if len(l.keys) > linkerMaxKeys {
		l.pruneLocked(now)
	}
}

// pruneLocked drops expired keys, then the oldest, until under the cap.
func (l *searchLinker) pruneLocked(now time.Time) {
	for k, recs := range l.keys {
		if now.Sub(recs[len(recs)-1].at) > linkContainedWindow {
			delete(l.keys, k)
		}
	}
	for len(l.keys) > linkerMaxKeys {
		var oldestKey string
		var oldest time.Time
		for k, recs := range l.keys {
			if t := recs[len(recs)-1].at; oldestKey == "" || t.Before(oldest) {
				oldestKey, oldest = k, t
			}
		}
		delete(l.keys, oldestKey)
	}
}

// link returns the search a call to name followed, if any: the most recent
// search within linkContainedWindow whose results contained name (with that
// rank), else the latest search within linkLatestWindow with rank 0.
func (l *searchLinker) link(key, name string, now time.Time) (id string, rank int, age time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	recs := l.keys[key]
	for i := len(recs) - 1; i >= 0; i-- {
		if r, hit := recs[i].ranks[name]; hit && now.Sub(recs[i].at) <= linkContainedWindow {
			return recs[i].id, r, now.Sub(recs[i].at), true
		}
	}
	if n := len(recs); n > 0 && now.Sub(recs[n-1].at) <= linkLatestWindow {
		return recs[n-1].id, 0, now.Sub(recs[n-1].at), true
	}
	return "", 0, 0, false
}
