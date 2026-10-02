package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

// ChannelService is shared by consumers on every transport. HTTP owns framing
// only; publication, authorization hooks, history and replay live in channels.
type ChannelService interface {
	List(context.Context, string) ([]channels.Channel, error)
	Publish(context.Context, gomsg.Envelope) (gomsg.Envelope, error)
	History(context.Context, string, string, int64, int) (channels.Page, error)
	Subscribe(context.Context, string, string, *int64) (<-chan channels.Event, error)
}

func (s *Server) registerChannelRoutes(router *http.ServeMux) {
	if s.Channels == nil {
		return
	}
	router.HandleFunc("/channels", s.handleChannelsList)
	router.HandleFunc("/channels/", s.handleChannelsItem)
}

func writeChannelError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, channels.ErrInvalid):
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
	case errors.Is(err, channels.ErrForbidden):
		writeError(w, http.StatusForbidden, CodeForbidden, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	}
}

func (s *Server) handleChannelsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	list, err := s.Channels.List(r.Context(), r.URL.Query().Get("as"))
	if err != nil {
		writeChannelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": list})
}

func (s *Server) handleChannelsItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/channels/"), "/")
	if len(parts) != 2 || (parts[1] != "messages" && parts[1] != "subscribe") {
		writeError(w, http.StatusNotFound, CodeNotFound, "unknown channel action")
		return
	}
	q := r.URL.Query()
	// Event IDs are durable publication sequences. Explicit since wins over
	// Last-Event-ID, matching normal browser EventSource reconnection behavior.
	rawSince := q.Get("since")
	if rawSince == "" && parts[1] == "subscribe" {
		rawSince = r.Header.Get("Last-Event-ID")
	}
	var since *int64
	if rawSince != "" {
		n, err := strconv.ParseInt(rawSince, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "since must be a non-negative publication sequence")
			return
		}
		since = &n
	}
	if parts[1] == "subscribe" {
		s.handleChannelSubscribe(w, r, parts[0], since)
		return
	}
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "limit must be between 1 and 1000")
			return
		}
		limit = n
	}
	cursor := int64(0)
	if since != nil {
		cursor = *since
	}
	page, err := s.Channels.History(r.Context(), parts[0], q.Get("as"), cursor, limit)
	if err != nil {
		writeChannelError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleChannelSubscribe(w http.ResponseWriter, r *http.Request, name string, since *int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "streaming not supported")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stream, err := s.Channels.Subscribe(ctx, name, r.URL.Query().Get("as"), since)
	if err != nil {
		writeChannelError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case event, open := <-stream:
			if !open || event.Err != nil {
				return // A reconnect replays from the last successfully written ID.
			}
			b, err := json.Marshal(event.Message)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", event.Message.Seq, b); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}
