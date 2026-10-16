package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/shimcodex"
	_ "modernc.org/sqlite"
)

func code(err error) string {
	if err == nil {
		return "ok"
	}
	var f *shimcodex.Failure
	if errors.As(err, &f) {
		return f.Code
	}
	return "unclassified_refusal"
}

// This one-shot diagnostic never opens Store, runtime, host, RPC or a journal.
// It reads one bounded protocol row and prints structural metadata only.
func main() {
	if len(os.Args) != 3 {
		fmt.Println("arguments_refused")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := url.URL{Scheme: "file", Path: os.Args[1], RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		fmt.Println("database_unavailable")
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var raw string
	if db.QueryRowContext(ctx, `SELECT state_json FROM codex_shim_protocol WHERE session_id=? AND length(CAST(state_json AS BLOB))<=67108864`, os.Args[2]).Scan(&raw) != nil {
		fmt.Println("protocol_unavailable")
		return
	}
	var s shimcodex.State
	if json.Unmarshal([]byte(raw), &s) != nil || len(s.Inbox) > 1024 {
		fmt.Println("protocol_refused")
		return
	}
	full, fullErr := shimcodex.BuildDeliveryProjection(s)
	out := map[string]any{"revision": s.Revision, "epoch": s.Epoch, "inbox_count": len(s.Inbox), "projection_code": code(fullErr), "candidate_active_turn": full.ActiveTurnID}
	if p := s.Delivery; p != nil {
		out["checkpoint"] = map[string]any{"accepted": p.AcceptedSourceCursor, "delivered": p.DeliveredHighWater, "base": p.BaseSourceCursor, "active_turn": p.ActiveTurnID, "active_thread": p.ActiveThreadID, "stdout_offset": p.StdoutOffset, "partial_start": p.PartialStart, "partial_bytes": len(p.PartialBytes), "turn_count": len(p.Turns)}
	}
	for i, e := range s.Inbox {
		next := s
		next.Inbox = s.Inbox[:i+1]
		next.Cursor = e.Cursor
		span := strings.Split(e.Identity, ":")
		if len(span) >= 4 && span[len(span)-3] == "stdout" {
			end, parseErr := strconv.ParseUint(span[len(span)-1], 10, 64)
			if parseErr == nil {
				next.StreamOffset = end
				next.PartialStart = end
				next.Partial = nil
			}
		}
		_, refusal := shimcodex.BuildDeliveryProjection(next)
		if refusal == nil {
			continue
		}
		m, decodeErr := shimcodex.Decode(e.Raw)
		failure := map[string]any{"index": i, "code": code(refusal), "source_id": e.Identity, "cursor": e.Cursor, "decode_code": code(decodeErr), "method": m.Method}
		var params map[string]json.RawMessage
		if json.Unmarshal(m.Params, &params) == nil {
			var item map[string]json.RawMessage
			if json.Unmarshal(params["item"], &item) == nil {
				types := map[string]string{}
				for key, value := range item {
					var decoded any
					if json.Unmarshal(value, &decoded) != nil {
						types[key] = "invalid"
						continue
					}
					switch decoded.(type) {
					case nil:
						types[key] = "null"
					case string:
						types[key] = "string"
					case float64:
						types[key] = "number"
					case bool:
						types[key] = "boolean"
					case []any:
						types[key] = "array"
					case map[string]any:
						types[key] = "object"
					}
				}
				failure["item_types"] = types
				var kind string
				if json.Unmarshal(item["type"], &kind) == nil {
					switch kind {
					case "userMessage", "agentMessage", "commandExecution":
						failure["item_type"] = kind
					default:
						failure["item_type"] = "unsupported_union"
					}
				}
			}
		}
		out["first_refusal"] = failure
		break
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}
