package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

const (
	mailboxWatchBatch     = 100
	mailboxWatchWriteWait = 15 * time.Second
)

// Package vars so tests can run the loop quickly.
var (
	mailboxWatchPoll = 500 * time.Millisecond
	mailboxWatchPing = 15 * time.Second
)

// mailboxWatchGap is the data of a "gap" event. Unlike an environment stream
// gap it never requires a snapshot: reconnect without after_seq to resume from
// the head, or read history with GET /messages/list.
type mailboxWatchGap struct {
	Reason       string `json:"reason"`
	HighWaterSeq int64  `json:"high_water_seq"`
}

// mailboxWatchMessage is one delivery: the same message projection as
// GET /messages/list (subject/body included) plus its cursor.
type mailboxWatchMessage struct {
	Seq int64 `json:"seq"`
	store.Message
}

// handleMessagesWatch services GET /messages/watch, the resumable mailbox
// stream (CW-20261012-0021). It speaks the ADR 0063 stream framing used by the
// environment streams (`id: <seq>`, `event: <kind>`, `gap`, `synchronized`)
// over durable mailbox rows. Reads are side-effect free like /messages/list:
// nothing is marked delivered or read, and no wake is triggered.
//
// Query params: to and as (required, equal, as for /messages/list), after_seq
// (or Last-Event-ID; absent means "from now"), kind, thread_id, unread_only.
//
// Events: "message" (id = seq), "synchronized" once the replay has caught up,
// and "gap" (reason "ahead" when after_seq is past the newest row, "restart"
// on a failed read), after which the stream closes. ": ping" comments keep
// idle proxies and SSH forwards open. Redelivery after a reconnect is bounded
// by the client's cursor; clients still dedupe by seq or message id.
func (s *Server) handleMessagesWatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	watcher, ok := s.MessageStore.(store.MailboxWatcher)
	if !ok {
		writeError(w, http.StatusNotFound, CodeNotFound, "mailbox watch not available")
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "streaming not supported")
		return
	}
	q := r.URL.Query()
	toURN, as := q.Get("to"), q.Get("as")
	if toURN == "" || as == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "to and as query params required")
		return
	}
	if as != toURN {
		writeError(w, http.StatusForbidden, CodeForbidden, "as must match to")
		return
	}
	to, err := messaging.ParseURN(toURN)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid to URN: "+err.Error())
		return
	}
	if _, ok := channels.AddressName(to); ok {
		writeChannelError(w, channels.ErrMailboxOperation)
		return
	}
	var f store.ListFilter
	if ks := q.Get("kind"); ks != "" {
		for _, k := range strings.Split(ks, ",") {
			f.Kind = append(f.Kind, messaging.Kind(strings.TrimSpace(k)))
		}
	}
	f.ThreadID = q.Get("thread_id")
	f.UnreadOnly = q.Get("unread_only") == "true"

	raw := q.Get("after_seq")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	after := int64(math.MaxInt64) // "from now": resolved to the high-water below
	if raw != "" {
		if after, err = strconv.ParseInt(raw, 10, 64); err != nil || after < 0 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid after_seq")
			return
		}
	}
	fromNow := raw == ""

	// First read before the headers go out so a store failure is a real 500.
	win, err := watcher.MailboxAfter(r.Context(), to, after, f, mailboxWatchBatch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	ctl := http.NewResponseController(w)
	emit := func(format string, args ...any) bool {
		_ = ctl.SetWriteDeadline(time.Now().Add(mailboxWatchWriteWait))
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return ctl.Flush() == nil
	}
	emitEvent := func(kind string, id int64, data any) bool {
		body, err := json.Marshal(data)
		if err != nil {
			return false
		}
		return emit("id: %d\nevent: %s\ndata: %s\n\n", id, kind, body)
	}

	if fromNow {
		after = win.HighWater
		win.Entries = nil
	} else if after > win.HighWater {
		emitEvent("gap", after, mailboxWatchGap{Reason: "ahead", HighWaterSeq: win.HighWater})
		return
	}
	synced := false
	ping := time.NewTicker(mailboxWatchPing)
	defer ping.Stop()
	poll := time.NewTicker(mailboxWatchPoll)
	defer poll.Stop()
	for {
		for _, e := range win.Entries {
			if !emitEvent("message", e.Seq, mailboxWatchMessage{Seq: e.Seq, Message: e.Message}) {
				return
			}
			after = e.Seq
		}
		if len(win.Entries) < mailboxWatchBatch {
			// Caught up: everything at or below HighWater was visible to this read.
			after = max(after, win.HighWater)
			if !synced {
				synced = true
				if !emitEvent("synchronized", after, struct {
					Seq int64 `json:"seq"`
				}{after}) {
					return
				}
			}
			select {
			case <-r.Context().Done():
				return
			case <-ping.C:
				if !emit(": ping\n\n") {
					return
				}
			case <-poll.C:
			}
		}
		if win, err = watcher.MailboxAfter(r.Context(), to, after, f, mailboxWatchBatch); err != nil {
			if r.Context().Err() == nil {
				emitEvent("gap", after, mailboxWatchGap{Reason: "restart", HighWaterSeq: after})
			}
			return
		}
	}
}
