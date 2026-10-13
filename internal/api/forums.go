package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	f "github.com/hollis-labs/go-forums"
	"github.com/hollis-labs/tether/internal/identity"
)

type ForumService interface {
	Execute(context.Context, f.Caller, string, f.Request) (any, error)
}

// forumCaller shares /auth/context's resolution. Attribution selectors never
// come from request bodies, query parameters or forwarded headers.
func (s *Server) forumCaller(ctx context.Context) (f.Caller, error) {
	p, ok := identity.FromContext(ctx)
	if !ok || p.ID == "" {
		return f.Caller{}, fmt.Errorf("%w: authenticate with an operator or session bearer credential", f.ErrForbidden)
	}
	snap := ResolveCallerContext(ctx, s.Service, s.Registry)
	c := f.Caller{Provenance: f.Provenance{Principal: f.Principal{ID: p.ID}, Verified: p.Verified}}
	switch p.Kind {
	case "operator":
		c.Kind = "user"
		c.Operator = p.Verified && p.ID == identity.OperatorID
	case "session":
		c.Kind = "agent"
		if snap.SessionResolved {
			c.SessionID = snap.SessionID
			c.AgentURN = snap.AgentURN
			c.WorkstreamID = snap.WorkstreamID
			c.ProjectID = snap.ProjectID
			c.LaunchID = snap.LaunchID
			if snap.AgentURN != "" {
				c.ID = snap.AgentURN
			}
		}
	default:
		return c, fmt.Errorf("%w: forums require an operator or session principal", f.ErrForbidden)
	}
	return c, nil
}
func (s *Server) registerForumRoutes(m *http.ServeMux) {
	if s.Forums == nil {
		return
	}
	m.HandleFunc("/forums", s.handleForums)
	m.HandleFunc("/forums/", s.handleForums)
}

// ForumRoute is the shared HTTP mapping used by daemon clients and MCP.
type forumRouteSpec struct{ op, method, path string }

func forumRoutes() []forumRouteSpec {
	routes := []forumRouteSpec{
		{"forum_create", "POST", "/forums"}, {"forum_list", "GET", "/forums"},
		{"category_create", "POST", "/forums/categories"}, {"category_list", "GET", "/forums/categories"},
		{"topic_create", "POST", "/forums/:forum_id/topics"}, {"topic_list", "GET", "/forums/:forum_id/topics"},
		{"post_create", "POST", "/forums/topics/:topic_id/posts"}, {"post_list", "GET", "/forums/topics/:topic_id/posts"},
		{"participant_list", "GET", "/forums/:forum_id/participants"}, {"participant_add", "POST", "/forums/:forum_id/participants"},
		{"participant_revoke", "POST", "/forums/:forum_id/participants/revoke"}, {"owner_set", "POST", "/forums/:forum_id/owner"},
		{"forum_search", "GET", "/forums/:forum_id/search"},
	}
	for _, entity := range []struct{ kind, path string }{{"category", "/forums/categories/:category_id"}, {"forum", "/forums/:forum_id"}, {"topic", "/forums/topics/:topic_id"}, {"post", "/forums/posts/:post_id"}} {
		routes = append(routes, forumRouteSpec{entity.kind + "_get", "GET", entity.path}, forumRouteSpec{entity.kind + "_update", "PATCH", entity.path}, forumRouteSpec{entity.kind + "_delete", "DELETE", entity.path})
		if entity.kind != "post" {
			for _, action := range []string{"archive", "unarchive"} {
				routes = append(routes, forumRouteSpec{entity.kind + "_" + action, "POST", entity.path + "/" + action})
			}
		}
		if entity.kind != "category" {
			for _, action := range []string{"move", "clone"} {
				routes = append(routes, forumRouteSpec{entity.kind + "_" + action, "POST", entity.path + "/" + action})
			}
		}
		if entity.kind == "topic" {
			routes = append(routes, forumRouteSpec{"topic_fork", "POST", entity.path + "/fork"})
		}
	}
	return routes
}
func ForumRoute(op string, r f.Request) (string, string, error) {
	for _, spec := range forumRoutes() {
		if spec.op == op {
			path := spec.path
			for key, id := range map[string]string{"forum_id": r.ForumID, "topic_id": r.TopicID, "post_id": r.PostID, "category_id": categoryID(r.CategoryID)} {
				if strings.Contains(path, ":"+key) {
					if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/%?#\\") {
						return "", "", f.ErrInvalid
					}
					path = strings.ReplaceAll(path, ":"+key, id)
				}
			}
			return spec.method, path, nil
		}
	}
	return "", "", f.ErrInvalid
}
func (s *Server) handleForums(w http.ResponseWriter, r *http.Request) {
	var req f.Request
	var op string
	var ids map[string]string
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for _, spec := range forumRoutes() {
		if spec.method != r.Method {
			continue
		}
		pattern := strings.Split(strings.Trim(spec.path, "/"), "/")
		if len(parts) != len(pattern) {
			continue
		}
		candidate := map[string]string{}
		match := true
		for i, key := range pattern {
			if strings.HasPrefix(key, ":") {
				candidate[key[1:]] = parts[i]
			} else if key != parts[i] {
				match = false
				break
			}
		}
		if match {
			op = spec.op
			ids = candidate
			break
		}
	}
	if op == "" {
		writeError(w, 404, CodeNotFound, "forum route not found")
		return
	}
	if r.Method == http.MethodGet {
		if e := forumReadOptions(r, &req); e != nil {
			writeForumError(w, e)
			return
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 512*1024)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if e := dec.Decode(&req); e != nil && (e != io.EOF || r.Method != http.MethodDelete) {
			writeForumError(w, fmt.Errorf("%w: invalid JSON body: %w", f.ErrInvalid, e))
			return
		}
		var trailing any
		if e := dec.Decode(&trailing); e != io.EOF {
			writeForumError(w, fmt.Errorf("%w: one JSON object required", f.ErrInvalid))
			return
		}
	}
	// Path identifiers always win over conflicting body selectors.
	for key, id := range ids {
		switch key {
		case "forum_id":
			req.ForumID = id
		case "topic_id":
			req.TopicID = id
		case "post_id":
			req.PostID = id
		case "category_id":
			req.CategoryID = &id
		}
	}
	c, e := s.forumCaller(r.Context())
	if e != nil {
		writeForumError(w, e)
		return
	}
	out, e := s.Forums.Execute(r.Context(), c, op, req)
	if e != nil {
		writeForumError(w, e)
		return
	}
	status := 200
	if strings.HasSuffix(op, "_create") || strings.HasSuffix(op, "_clone") || strings.HasSuffix(op, "_fork") {
		status = 201
	}
	writeJSON(w, status, out)
}
func forumReadOptions(r *http.Request, v *f.Request) error {
	q := r.URL.Query()
	v.Namespace = q.Get("namespace")
	if q.Has("category_id") {
		id := q.Get("category_id")
		v.CategoryID = &id
	}
	if q.Has("archived") {
		b, e := strconv.ParseBool(q.Get("archived"))
		if e != nil {
			return fmt.Errorf("%w: archived must be a boolean", f.ErrInvalid)
		}
		v.Archived = &b
	}
	v.Cursor = q.Get("cursor")
	v.Status = q.Get("status")
	v.Author = q.Get("author")
	v.AuthorKind = q.Get("author_kind")
	v.CreatedAfter = q.Get("created_after")
	v.CreatedBefore = q.Get("created_before")
	v.WorkstreamID = q.Get("workstream_id")
	v.ProjectID = q.Get("project_id")
	v.Detail = q.Get("detail")
	v.Fields = q.Get("fields")
	v.Query = q.Get("query")
	for key, dest := range map[string]*int{"limit": &v.Limit, "max_chars": &v.MaxChars} {
		if raw := q.Get(key); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil {
				return fmt.Errorf("%w: %s must be an integer", f.ErrInvalid, key)
			}
			*dest = n
		}
	}
	if raw := q.Get("since_seq"); raw != "" {
		n, e := strconv.ParseInt(raw, 10, 64)
		if e != nil {
			return fmt.Errorf("%w: since_seq must be an integer", f.ErrInvalid)
		}
		v.SinceSeq = n
	}
	if raw := q.Get("has_replies"); raw != "" {
		b, e := strconv.ParseBool(raw)
		if e != nil {
			return fmt.Errorf("%w: has_replies must be a boolean", f.ErrInvalid)
		}
		v.HasReplies = &b
	}
	return nil
}
func writeForumError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, f.ErrInvalid):
		writeError(w, 400, CodeInvalidRequest, e.Error())
	case errors.Is(e, f.ErrForbidden):
		writeError(w, 403, CodeForbidden, e.Error())
	case errors.Is(e, f.ErrNotFound):
		writeError(w, 404, CodeNotFound, e.Error())
	case errors.Is(e, f.ErrConflict):
		writeError(w, 409, CodeConflict, e.Error())
	default:
		writeError(w, 500, CodeInternalError, "forum operation failed")
	}
}

func categoryID(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
