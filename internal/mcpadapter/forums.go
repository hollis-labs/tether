package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	f "github.com/hollis-labs/libs/core/forums"
	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
)

func (a *Adapter) registerForumTools(s *gomcp.Server) {
	for _, spec := range []struct {
		op, description string
		write           bool
	}{
		{"category_create", "Create one category in a consumer namespace; no nested categories. Operator only.", true},
		{"category_get", "Read a visible category.", false},
		{"category_list", "List visible categories with namespace and archive filters.", false},
		{"category_update", "Update category title or description. Operator only.", true},
		{"category_delete", "Delete an empty category; cascade=true explicitly deletes all contained boards and content. Operator only.", true},
		{"category_archive", "Archive category; descendant writes are blocked while reads remain available.", true},
		{"category_unarchive", "Restore category; independently archived descendants stay archived.", true},
		{"forum_update", "Update board title, description, config and metadata. Owner or operator.", true},
		{"forum_delete", "Delete an empty board; cascade=true explicitly deletes topics and posts.", true},
		{"forum_archive", "Archive board and block descendant writes.", true},
		{"forum_unarchive", "Restore board; independently archived topics stay archived.", true},
		{"forum_move", "Move board within its namespace to destination_category_id; empty string places it at root.", true},
		{"forum_clone", "Deep-copy board and content with new IDs. Supply new id; optional destination_category_id. Clone belongs to caller; participant grants are not copied.", true},
		{"topic_update", "Update thread title or open/resolved status. Owner or operator.", true},
		{"topic_delete", "Delete an empty thread; cascade=true explicitly deletes its posts.", true},
		{"topic_archive", "Archive thread without losing open/resolved status.", true},
		{"topic_unarchive", "Restore thread and its previous open/resolved status.", true},
		{"topic_move", "Move entire thread to destination_forum_id within its namespace, retaining IDs and attribution.", true},
		{"topic_clone", "Copy entire thread to destination_forum_id with new IDs and source references.", true},
		{"topic_fork", "Copy post_id and its replies from topic_id into a new thread in destination_forum_id. Source is retained.", true},
		{"post_update", "Edit message body_md, subject or attachment references; original attribution is retained. Owner or operator.", true},
		{"post_delete", "Delete a message; cascade=true is required to delete its reply subtree.", true},
		{"post_move", "Move a message and its replies to destination_topic_id; optional parent_post_id reparents the subtree. IDs are retained.", true},
		{"post_clone", "Copy a message and its replies to destination_topic_id with new IDs and source references; optional parent_post_id.", true},
		{"forum_create", "Register a durable discussion forum as the operator. Namespace: app/<consumer>/forum/<instance>; multiple boards share a namespace. Optional category_id groups boards. Use forums for topics and threaded discussion, groups for chat, messages for delivery, and channels for event history.", true},
		{"participant_list", "Operator only: list explicit stable-principal grants with bounded pagination.", false},
		{"participant_add", "Operator only: grant read or post (includes read) access to an authenticated stable principal, or update its grant.", true},
		{"participant_revoke", "Operator only: revoke a grant; subsequent requests and cursor continuations recheck access. Owners retain implicit access.", true},
		{"owner_set", "Operator only: transfer forum ownership; omitted owner defaults to operator. Invite the old owner explicitly before transfer to retain access.", true},
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
		if strings.HasSuffix(op, "_delete") {
			behavior = Destroys("permanently deletes forum content and, with cascade, descendants and correlation refs")
		}
		if strings.HasSuffix(op, "_update") {
			behavior = Destroys("overwrites selected forum fields; prior values are not versioned")
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
	if schema := forumManagementSchema(op); schema != nil {
		return schema
	}
	p := []gomcp.Prop{}
	switch op {
	case "forum_create":
		p = append(p, gomcp.StringProp("id", "Forum id (letters, digits, underscore or dash)", true), gomcp.StringProp("namespace", "app/<consumer>/forum/<instance>", true), gomcp.StringProp("title", "Forum title (1..300 characters)", true), gomcp.StringProp("owner_type", "Optional owner kind: user or agent; operator registration only", false), gomcp.StringProp("owner_id", "Optional owner principal id (pair with owner_type); defaults to operator", false), gomcp.ObjectProp("config_json", "Opaque forum configuration; ui is reserved", false), gomcp.ObjectProp("metadata_json", "Opaque metadata", false))
	case "forum_get", "topic_create", "topic_list", "forum_search", "participant_list", "participant_add", "participant_revoke", "owner_set":
		p = append(p, gomcp.StringProp("forum_id", "Forum id", true))
	case "topic_get", "post_create", "post_list":
		p = append(p, gomcp.StringProp("topic_id", "Topic id", true))
	case "post_get":
		p = append(p, gomcp.StringProp("post_id", "Post id", true))
	}
	if op == "participant_add" || op == "participant_revoke" {
		p = append(p, gomcp.StringProp("principal_id", "Stable authenticated principal id; agents use their resolved agent URN", true), gomcp.StringProp("principal_kind", "user or agent", true))
	}
	if op == "participant_add" {
		p = append(p, gomcp.StringProp("access", "read or post (includes read)", true))
	}
	if op == "owner_set" {
		p = append(p, gomcp.StringProp("owner_type", "user or agent; omit both owner fields for operator", false), gomcp.StringProp("owner_id", "Stable owner principal id", false))
	}
	if op == "topic_create" {
		p = append(p, gomcp.StringProp("title", "Topic title (1..300 characters)", true))
	}
	if op == "forum_create" {
		p = append(p, forumSortProp())
		p = append(p, gomcp.StringProp("category_id", "Optional category; omit for root board", false), gomcp.StringProp("description", "Board description", false))
	}
	if op == "forum_list" {
		p = append(p, gomcp.StringProp("namespace", "Exact namespace filter", false), gomcp.StringProp("category_id", "Category filter; empty string selects root boards", false), gomcp.BooleanProp("archived", "Optional archive filter; omitted includes both", false))
	}
	if op == "topic_list" {
		p = append(p, gomcp.BooleanProp("archived", "Optional archive filter", false))
	}
	if op == "post_create" {
		p = append(p, forumMessageProps()...)
	}
	if op == "post_create" {
		p = append(p, gomcp.StringProp("body_md", "Markdown body (1..100000 characters)", true), gomcp.StringProp("parent_post_id", "Optional parent post in this topic", false))
	}
	if op == "forum_search" {
		p = append(p, gomcp.StringProp("query", "Search text (literal words, not FTS syntax)", true))
	}
	if op != "forum_create" && op != "topic_create" && op != "post_create" && op != "participant_add" && op != "participant_revoke" && op != "owner_set" {
		p = append(p, gomcp.StringProp("cursor", "Opaque next_cursor from the same operation and filters", false), gomcp.IntegerProp("limit", "Page size, default 20, maximum 100", false), gomcp.IntegerProp("since_seq", "Only posts newer than this per-forum sequence", false), gomcp.StringProp("detail", "summary (default), preview, full; post_get bodies only", false), gomcp.StringProp("fields", "Comma-separated record fields; id and page metadata survive", false), gomcp.IntegerProp("max_chars", "Aggregate body/snippet character budget, maximum 100000", false), gomcp.StringProp("status", "Topic status: open, resolved, archived", false), gomcp.StringProp("author", "Author principal or agent URN", false), gomcp.StringProp("author_kind", "user or agent", false), gomcp.StringProp("created_after", "Inclusive RFC3339 date", false), gomcp.StringProp("created_before", "Inclusive RFC3339 date", false), gomcp.BooleanProp("has_replies", "Filter records with replies", false), gomcp.StringProp("workstream_id", "Stamped workstream filter", false), gomcp.StringProp("project_id", "Stamped project filter", false))
	}
	return gomcp.InputSchema(p...)
}

func forumMessageProps() []gomcp.Prop {
	return []gomcp.Prop{gomcp.StringProp("subject", "Optional message subject (up to 300 characters)", false), gomcp.ArrayProp("attachments", "Up to 20 external attachment references; the host does not upload or fetch them", false, map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}, "uri": map[string]interface{}{"type": "string"}, "media_type": map[string]interface{}{"type": "string"}}, "required": []string{"name", "uri"}, "additionalProperties": false})}
}
func forumSortProp() gomcp.Prop {
	return gomcp.IntegerProp("sort_order", "Signed JSON-safe integer; lower sorts first, ties use id; default 0 on create, preserved when omitted on update/move/clone", false)
}

func forumManagementSchema(op string) map[string]interface{} {
	p := []gomcp.Prop{}
	switch op {
	case "category_create":
		p = append(p, gomcp.StringProp("id", "Category id, unique within host", true), gomcp.StringProp("namespace", "app/<consumer>/forum/<instance>", true), gomcp.StringProp("title", "Category title", true), gomcp.StringProp("description", "Description", false))
	case "category_get", "category_update", "category_delete", "category_archive", "category_unarchive":
		p = append(p, gomcp.StringProp("category_id", "Category id", true))
	case "category_list":
		p = append(p, gomcp.StringProp("namespace", "Exact namespace filter", false), gomcp.BooleanProp("archived", "Optional archive filter", false), gomcp.IntegerProp("limit", "Default 20, maximum 100", false), gomcp.StringProp("cursor", "Continuation cursor", false))
	case "forum_update", "forum_delete", "forum_archive", "forum_unarchive", "forum_move", "forum_clone":
		p = append(p, gomcp.StringProp("forum_id", "Board id", true))
	case "topic_update", "topic_delete", "topic_archive", "topic_unarchive", "topic_move", "topic_clone", "topic_fork":
		p = append(p, gomcp.StringProp("topic_id", "Thread id", true))
	case "post_update", "post_delete", "post_move", "post_clone":
		p = append(p, gomcp.StringProp("post_id", "Message id", true))
	default:
		return nil
	}
	if strings.HasSuffix(op, "_delete") {
		p = append(p, gomcp.BooleanProp("cascade", "Explicitly delete descendants; default false refuses nonempty containers", false))
	}
	if op == "category_update" || op == "forum_update" || op == "topic_update" || op == "forum_clone" || op == "topic_clone" || op == "topic_fork" {
		p = append(p, gomcp.StringProp("title", "New title (omitted keeps existing)", false))
	}
	if op == "category_update" || op == "forum_update" {
		p = append(p, gomcp.StringProp("description", "New description; empty clears", false))
	}
	if op == "forum_update" {
		p = append(p, gomcp.ObjectProp("config_json", "Replace consumer configuration", false), gomcp.ObjectProp("metadata_json", "Replace consumer metadata", false))
	}
	if op == "topic_update" {
		p = append(p, gomcp.StringProp("status", "open or resolved", false))
	}
	if op == "category_create" || op == "category_update" || op == "forum_update" || op == "forum_move" || op == "forum_clone" {
		p = append(p, forumSortProp())
	}
	if op == "forum_move" || op == "forum_clone" {
		p = append(p, gomcp.StringProp("destination_category_id", "Category in same namespace; empty string means root", op == "forum_move"))
	}
	if op == "forum_clone" {
		p = append(p, gomcp.StringProp("id", "New board id", true))
	}
	if op == "topic_move" || op == "topic_clone" || op == "topic_fork" {
		p = append(p, gomcp.StringProp("destination_forum_id", "Target board in same namespace", true))
	}
	if op == "topic_fork" {
		p = append(p, gomcp.StringProp("post_id", "Root of reply subtree to copy", true))
	}
	if op == "post_move" || op == "post_clone" {
		p = append(p, gomcp.StringProp("destination_topic_id", "Target thread in same namespace", true), gomcp.StringProp("parent_post_id", "Parent in destination; omitted makes subtree a root", false))
	}
	if op == "post_update" {
		p = append(p, gomcp.StringProp("body_md", "Replacement body (1..100000 characters)", false))
		p = append(p, forumMessageProps()...)
	}
	return gomcp.InputSchema(p...)
}
