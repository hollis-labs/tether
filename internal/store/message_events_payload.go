package store

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

func messageMetadataEvent(kind, raw string, at time.Time, team string) events.Event {
	schema := "urn:hollis-labs:events:message.lifecycle@1"
	category := "message"
	if strings.HasPrefix(kind, "wake.") {
		schema = "urn:hollis-labs:events:wake@1"
		category = "wake"
	}
	var ids struct {
		MessageID string   `json:"message_id"`
		ThreadID  string   `json:"thread_id"`
		InReplyTo string   `json:"in_reply_to"`
		Group     string   `json:"group"`
		GroupSeq  int64    `json:"group_seq"`
		To        []string `json:"to"`
	}
	_ = json.Unmarshal([]byte(raw), &ids)
	if category == "message" && len(ids.To) > 0 {
		// These are existing read endpoints. since_seq=0 means the member's
		// CURRENT cursor, so the first post deliberately uses -1 to remain
		// addressable after that cursor advances. No invented message_id filter.
		q := url.Values{"as": []string{ids.To[0]}}
		ref := "/messages/" + url.PathEscape(ids.MessageID)
		if ids.Group != "" {
			since := ids.GroupSeq - 1
			if since == 0 {
				since = -1
			}
			q.Set("since_seq", strconv.FormatInt(since, 10))
			q.Set("limit", "1")
			ref = "/groups/" + url.PathEscape(ids.Group) + "/messages"
		}
		var payload map[string]any
		if json.Unmarshal([]byte(raw), &payload) == nil {
			payload["body_ref"] = ref + "?" + q.Encode()
			if encoded, err := json.Marshal(payload); err == nil {
				raw = string(encoded)
			}
		}
	}
	return events.Event{Scope: events.ScopeDaemon, Kind: kind, At: at, PayloadJSON: raw, Envelope: events.Envelope{Dims: events.ScopeDims{TeamRunID: team}, Corr: events.Corr{CorrelationID: ids.MessageID, ThreadID: ids.ThreadID, InReplyTo: ids.InReplyTo}, Class: events.Class{Category: category, PayloadSchema: schema, Sensitivity: "private", Visibility: "environment"}}}
}
