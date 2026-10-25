package mcpadapter

// Owner-side paging for the Tether tools that list a lot of records
// (CW-20261024-0053). The gateway result cap (CW-20261024-0052) is the safety net;
// this is the fix at the source: a tool that lists records takes limit and cursor,
// returns brief rows by default, and says how to get the rest. A call must never
// succeed while ignoring an argument the caller supplied (CW-20261024-0039), so
// these tools are registered with strictArgs as well.

import (
	"encoding/base64"
	"fmt"
	"sort"
)

const (
	// defaultListLimit is the page size when the caller names none. With brief rows it
	// keeps a page well under the gateway cap on today's data.
	defaultListLimit = 25
	// maxListLimit is the largest page a caller may ask for.
	maxListLimit = 200
)

// listPage is the parsed paging contract of a list tool call.
type listPage struct {
	Limit int
	// After is the sort key of the last item of the previous page; empty on the first page.
	After string
	// Verbose asks for full records instead of brief rows.
	Verbose bool
}

// parseListPage reads limit, cursor and verbose. An invalid limit or cursor is refused
// rather than replaced by a default, so a caller who asked for something else is told.
func parseListPage(args map[string]any) (listPage, error) {
	p := listPage{Limit: defaultListLimit, Verbose: boolArg(args, "verbose")}
	if raw, present := args["limit"]; present && raw != nil {
		n := intArg(args, "limit", -1)
		if n < 1 || n > maxListLimit {
			return p, toolError("invalid_request", fmt.Sprintf("limit must be an integer from 1 to %d", maxListLimit)).WithField("limit")
		}
		p.Limit = n
	}
	if c := str(args, "cursor"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil || len(raw) == 0 {
			return p, toolError("invalid_request", "cursor is not one this tool returned; omit it to start from the first page").WithField("cursor")
		}
		p.After = string(raw)
	}
	return p, nil
}

// window returns the half-open range of keys (unique, sorted ascending) that follows the
// cursor, and the cursor for the next page ("" on the last page). The cursor is the
// last key returned, so a page stays correct if records are added or removed between
// calls; it is opaque to the caller.
func (p listPage) window(keys []string) (start, end int, next string) {
	if p.After != "" {
		start = sort.Search(len(keys), func(i int) bool { return keys[i] > p.After })
	}
	end = start + p.Limit
	if end > len(keys) {
		end = len(keys)
	}
	if end < len(keys) && end > start {
		next = base64.RawURLEncoding.EncodeToString([]byte(keys[end-1]))
	}
	return start, end, next
}

// pageEnvelope adds the paging fields every list tool reports: how many rows this page
// holds, how many records match in all, the cursor for the rest, and whether the rows
// are brief. Callers add their own rows key.
func pageEnvelope(out map[string]any, returned, total int, next string, verbose bool) map[string]any {
	out["count"] = returned
	out["total"] = total
	if next != "" {
		out["next_cursor"] = next
	}
	if verbose {
		out["detail"] = "full"
	} else {
		out["detail"] = "brief"
	}
	return out
}
