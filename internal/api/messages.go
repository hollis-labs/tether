package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hollis-labs/go-messaging"
)

// MessageStore is the seam the /messages/* handlers depend on.
// *store.Store.MessagingStore() satisfies it.
type MessageStore interface {
	messaging.Store
}

// registerMessageRoutes mounts the go-messaging-native HTTP surface.
// No-op when MessageStore is nil.
func (s *Server) registerMessageRoutes(mux *http.ServeMux) {
	if s.MessageStore == nil {
		return
	}
	mux.HandleFunc("/messages", s.handleMessagesCollection)
	mux.HandleFunc("/messages/request", s.handleMessageRequest)
	mux.HandleFunc("/messages/inbox", s.handleMessagesInbox)
	mux.HandleFunc("/messages/thread/", s.handleMessagesThread)
	mux.HandleFunc("/messages/", s.handleMessagesItem)
}

func (s *Server) handleMessagesCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleMessageSend(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleMessagesItem(w http.ResponseWriter, r *http.Request) {
	// /messages/inbox → handleMessagesInbox (already registered as exact path)
	// /messages/thread/{id} → handleMessagesThread (registered with prefix)
	rest := strings.TrimPrefix(r.URL.Path, "/messages/")
	if rest == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "message id required")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch action {
	case "":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMessageGet(w, r, id)
	case "consume":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMessageConsume(w, r, id)
	case "cancel":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMessageCancel(w, r, id)
	default:
		writeError(w, http.StatusNotFound, CodeNotFound, "unknown action "+action)
	}
}

// POST /messages
// Body: messaging.Envelope (id, created_at, delivered_at, consumed_at are server-assigned/ignored).
func (s *Server) handleMessageSend(w http.ResponseWriter, r *http.Request) {
	var env messaging.Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
		return
	}
	// Guard: reject caller-set lifecycle fields.
	env.ID = ""
	env.CreatedAt = time.Time{}

	sent, err := s.MessageStore.Send(r.Context(), env)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sent)
}

// GET /messages/{id}
func (s *Server) handleMessageGet(w http.ResponseWriter, r *http.Request, id string) {
	env, err := s.MessageStore.Get(r.Context(), id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, CodeNotFound, "message not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, env)
}

// GET /messages/inbox?to=<urn>[&kind=request,notice][&thread_id=X][&limit=N]
func (s *Server) handleMessagesInbox(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	toURN := q.Get("to")
	if toURN == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "to query param required")
		return
	}
	to, err := messaging.ParseURN(toURN)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid to URN: "+err.Error())
		return
	}

	var f messaging.Filter
	if ks := q.Get("kind"); ks != "" {
		for _, k := range strings.Split(ks, ",") {
			f.Kind = append(f.Kind, messaging.Kind(strings.TrimSpace(k)))
		}
	}
	f.ThreadID = q.Get("thread_id")

	envs, err := s.MessageStore.Inbox(r.Context(), to, f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": envs})
}

// GET /messages/thread/{thread_id}[?kind=X][&limit=N]
func (s *Server) handleMessagesThread(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	threadID := strings.TrimPrefix(r.URL.Path, "/messages/thread/")
	if threadID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "thread_id required")
		return
	}

	var f messaging.Filter
	if ks := r.URL.Query().Get("kind"); ks != "" {
		for _, k := range strings.Split(ks, ",") {
			f.Kind = append(f.Kind, messaging.Kind(strings.TrimSpace(k)))
		}
	}

	envs, err := s.MessageStore.Thread(r.Context(), threadID, f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": envs})
}

// POST /messages/{id}/consume?as=<urn>
func (s *Server) handleMessageConsume(w http.ResponseWriter, r *http.Request, id string) {
	asURN := r.URL.Query().Get("as")
	if asURN == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as query param required")
		return
	}
	recipient, err := messaging.ParseURN(asURN)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid as URN: "+err.Error())
		return
	}
	if err := s.MessageStore.Consume(r.Context(), id, recipient); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /messages/{id}/cancel
func (s *Server) handleMessageCancel(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.MessageStore.Cancel(r.Context(), id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, CodeNotFound, "message not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /messages/request — blocking request/reply helper.
// Creates a request envelope and blocks until a matching response arrives
// or the timeout elapses (default 30s). Query params same as /broker/requests.
func (s *Server) handleMessageRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}

	q := r.URL.Query()
	var timeoutDur = defaultRequestTimeout
	if t := q.Get("timeout"); t != "" {
		d, err := time.ParseDuration(t)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid timeout: "+err.Error())
			return
		}
		timeoutDur = d
	}

	var env messaging.Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
		return
	}

	disp := messaging.NewDispatcher(s.MessageStore)
	waitCtx, cancel := context.WithTimeout(r.Context(), timeoutDur)
	defer cancel()

	resp, err := disp.Request(waitCtx, env)
	if err != nil {
		writeError(w, http.StatusGatewayTimeout, "timeout",
			"no response within "+timeoutDur.String())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func isNotFound(err error) bool {
	return errors.Is(err, messaging.ErrNotFound) ||
		strings.Contains(err.Error(), "no rows")
}
