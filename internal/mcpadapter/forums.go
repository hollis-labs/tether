package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	f "github.com/hollis-labs/go-forums"
	gomcp "github.com/hollis-labs/libs/plugin-mcp/go-mcp/server"
)

func (a *Adapter) registerForumTools(s *gomcp.Server) {
	for _, spec := range []struct {
		op, description string
		write           bool
	}{
		{"forum_create", "Register a durable discussion forum as the operator. Namespace: app/<consumer>/forum/<id>. Use forums for topics and threaded discussion, groups for chat, messages for delivery, and channels for event history.", true},
		{"forum_list", "List accessible forums in compact pages; expand one with tether_forum_get.", false},
		{"forum_get", "Read a forum's configuration and policy, then discover topics with tether_forum_topic_list.", false},
		{"topic_create", "Create an open discussion topic in a forum. The daemon stamps the author; add the first post separately.", true},
		{"topic_list", "List compact topic rows with status, author, date and provenance filters. Resume with next_cursor.", false},
		{"topic_get", "Read topic metadata and a paginated post outline (ids, authors, depth, short previews). Open selected bodies with tether_forum_post_get.", false},
		{"post_create", "Write Markdown to a topic; parent_post_id creates a reply within that topic. Author and session provenance are stamped by the daemon.", true},
		{"post_list", "Read compact post previews in sequence order. since_seq fetches newer posts; next_cursor continues a bounded page.", false},
		{"post_get", "Open a selected post: detail=summary (160 characters), preview (2000), or full (up to 100000). Inspect truncated and body_chars.", false},
		{"forum_search", "Search topic titles and post bodies within one accessible forum. Returns compact matched posts; open selected results with tether_forum_post_get.", false},
	} {
		op := spec.op
		// Each operation receives only its own identifiers and payload; attribution
		// is deliberately absent from the input schema.
		schema := forumToolSchema(op)
		behavior := Reads("daemon-owned forum read; no mailbox mutations")
		if spec.write {
			behavior = Writes()
		}
		a.addTool(s, gomcp.Tool{Name: "tether_forum_" + strings.TrimPrefix(op, "forum_"), Description: spec.description, InputSchema: schema, Handler: func(ctx context.Context, args map[string]any) (any, error) {
			if a.client == nil {
				return nil, toolError("daemon_required", "forums require the daemon HTTP client")
			}
			b, e := json.Marshal(args)
			if e != nil {
				return nil, toolError("invalid_request", e.Error())
			}
			var r f.Request
			decoder := json.NewDecoder(bytes.NewReader(b))
			decoder.DisallowUnknownFields()
			if e = decoder.Decode(&r); e != nil {
				return nil, toolError("invalid_request", e.Error())
			}
			out, e := a.client.ForumExecute(ctx, op, r)
			if e != nil {
				if strings.Contains(e.Error(), "(forbidden)") {
					return nil, toolError("forbidden", e.Error())
				}
				return nil, channelConsumerError(e)
			}
			return toolJSON(out), nil
		}}, behavior)
	}
}

func forumToolSchema(op string) map[string]interface{} {
	p := []gomcp.Prop{}
	switch op {
	case "forum_create":
		p = append(p, gomcp.StringProp("id", "Forum id (letters, digits, underscore or dash)", true), gomcp.StringProp("namespace", "app/<consumer>/forum/<id>", true), gomcp.StringProp("title", "Forum title (1..300 characters)", true), gomcp.StringProp("owner_type", "Optional owner kind: user or agent; operator registration only", false), gomcp.StringProp("owner_id", "Optional owner principal id (pair with owner_type); defaults to operator", false), gomcp.ObjectProp("config_json", "Opaque forum configuration; ui is reserved", false), gomcp.ObjectProp("metadata_json", "Opaque metadata", false))
	case "forum_get", "topic_create", "topic_list", "forum_search":
		p = append(p, gomcp.StringProp("forum_id", "Forum id", true))
	case "topic_get", "post_create", "post_list":
		p = append(p, gomcp.StringProp("topic_id", "Topic id", true))
	case "post_get":
		p = append(p, gomcp.StringProp("post_id", "Post id", true))
	}
	if op == "topic_create" {
		p = append(p, gomcp.StringProp("title", "Topic title (1..300 characters)", true))
	}
	if op == "post_create" {
		p = append(p, gomcp.StringProp("body_md", "Markdown body (1..100000 characters)", true), gomcp.StringProp("parent_post_id", "Optional parent post in this topic", false))
	}
	if op == "forum_search" {
		p = append(p, gomcp.StringProp("query", "Search text (literal words, not FTS syntax)", true))
	}
	if op != "forum_create" && op != "topic_create" && op != "post_create" {
		p = append(p, gomcp.StringProp("cursor", "Opaque next_cursor from the same operation and filters", false), gomcp.IntegerProp("limit", "Page size, default 20, maximum 100", false), gomcp.IntegerProp("since_seq", "Only posts newer than this per-forum sequence", false), gomcp.StringProp("detail", "summary (default), preview, full; post_get bodies only", false), gomcp.StringProp("fields", "Comma-separated record fields; id and page metadata survive", false), gomcp.IntegerProp("max_chars", "Aggregate body/snippet character budget, maximum 100000", false), gomcp.StringProp("status", "Topic status: open, resolved, archived", false), gomcp.StringProp("author", "Author principal or agent URN", false), gomcp.StringProp("author_kind", "user or agent", false), gomcp.StringProp("created_after", "Inclusive RFC3339 date", false), gomcp.StringProp("created_before", "Inclusive RFC3339 date", false), gomcp.BooleanProp("has_replies", "Filter records with replies", false), gomcp.StringProp("workstream_id", "Stamped workstream filter", false), gomcp.StringProp("project_id", "Stamped project filter", false))
	}
	return gomcp.InputSchema(p...)
}
