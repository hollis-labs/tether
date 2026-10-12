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
func ForumRoute(op string, r f.Request) (method, path string, err error) {
	switch op {
	case "forum_create":
		return "POST", "/forums", nil
	case "forum_list":
		return "GET", "/forums", nil
	case "forum_get":
		return "GET", "/forums/" + r.ForumID, nil
	case "topic_create":
		return "POST", "/forums/" + r.ForumID + "/topics", nil
	case "topic_list":
		return "GET", "/forums/" + r.ForumID + "/topics", nil
	case "topic_get":
		return "GET", "/forums/topics/" + r.TopicID, nil
	case "post_create":
		return "POST", "/forums/topics/" + r.TopicID + "/posts", nil
	case "post_list":
		return "GET", "/forums/topics/" + r.TopicID + "/posts", nil
	case "post_get":
		return "GET", "/forums/posts/" + r.PostID, nil
	case "forum_search":
		return "GET", "/forums/" + r.ForumID + "/search", nil
	}
	return "", "", f.ErrInvalid
}
func (s *Server) handleForums(w http.ResponseWriter, r *http.Request) {
	var req f.Request
	var op string
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// Read filters are accepted on GET only; strict JSON rejects caller provenance.
	switch r.Method {
	case http.MethodGet:
		if e := forumReadOptions(r, &req); e != nil {
			writeForumError(w, e)
			return
		}
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 512*1024)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if e := dec.Decode(&req); e != nil {
			writeForumError(w, fmt.Errorf("%w: invalid JSON body: %w", f.ErrInvalid, e))
			return
		}
		var trailing any
		if e := dec.Decode(&trailing); e != io.EOF {
			writeForumError(w, fmt.Errorf("%w: one JSON object required", f.ErrInvalid))
			return
		}
	default:
		writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
		return
	}
	switch {
	case len(parts) == 1:
		if r.Method == "GET" {
			op = "forum_list"
		} else {
			op = "forum_create"
		}
	case len(parts) == 3 && parts[1] == "topics" && r.Method == "GET":
		op = "topic_get"
		req.TopicID = parts[2]
	case len(parts) == 3 && parts[1] == "posts" && r.Method == "GET":
		op = "post_get"
		req.PostID = parts[2]
	case len(parts) == 4 && parts[1] == "topics" && parts[3] == "posts":
		req.TopicID = parts[2]
		if r.Method == "GET" {
			op = "post_list"
		} else {
			op = "post_create"
		}
	case len(parts) == 2 && r.Method == "GET":
		op = "forum_get"
		req.ForumID = parts[1]
	case len(parts) == 3 && parts[2] == "topics":
		req.ForumID = parts[1]
		if r.Method == "GET" {
			op = "topic_list"
		} else {
			op = "topic_create"
		}
	case len(parts) == 3 && parts[2] == "search" && r.Method == "GET":
		op = "forum_search"
		req.ForumID = parts[1]
	default:
		writeError(w, 404, CodeNotFound, "forum route not found")
		return
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
	if r.Method == "POST" {
		status = 201
	}
	writeJSON(w, status, out)
}
func forumReadOptions(r *http.Request, v *f.Request) error {
	q := r.URL.Query()
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
