package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// mailboxWatchIdle bounds how long a stream may be silent before the client
// treats it as dead (the server pings every 15s). A var so tests can shrink it.
var mailboxWatchIdle = 45 * time.Second

// MailboxWatchStatusError is a non-200 answer to the watch request. Callers
// retry 5xx (a proxy or restarting hub) and give up on the rest (404 means the
// hub has no such route).
type MailboxWatchStatusError struct {
	Status int
	Err    error
}

func (e *MailboxWatchStatusError) Error() string { return e.Err.Error() }
func (e *MailboxWatchStatusError) Unwrap() error { return e.Err }

// MailboxWatchQuery selects one recipient's stream. AfterSeq nil means "from
// now"; a non-nil cursor resumes strictly after it.
type MailboxWatchQuery struct {
	To         string
	AfterSeq   *int64
	Kind       string
	ThreadID   string
	UnreadOnly bool
}

// MailboxWatchEvent is one SSE frame of GET /messages/watch. Type is
// "message", "synchronized" or "gap"; keepalive comments are not surfaced.
type MailboxWatchEvent struct {
	Type    string
	Seq     int64
	Message MailboxWatchMessage
	Reason  string // gap only
}

// MailboxWatchMessage is a delivery: the /messages/list projection plus Seq.
type MailboxWatchMessage struct {
	MessageEnvelopeDTO
	Seq int64 `json:"seq"`
}

// MailboxWatch opens the stream once. It does not reconnect; the caller owns
// resume (see cmd/tether mailbox watch). The stream never marks anything
// delivered or read. The returned channels close when the stream ends; a
// transport failure is sent on the error channel first.
func (c *Client) MailboxWatch(ctx context.Context, q MailboxWatchQuery) (<-chan MailboxWatchEvent, <-chan error, error) {
	params := url.Values{}
	params.Set("to", q.To)
	params.Set("as", q.To)
	if q.AfterSeq != nil {
		params.Set("after_seq", strconv.FormatInt(*q.AfterSeq, 10))
	}
	if q.Kind != "" {
		params.Set("kind", q.Kind)
	}
	if q.ThreadID != "" {
		params.Set("thread_id", q.ThreadID)
	}
	if q.UnreadOnly {
		params.Set("unread_only", "true")
	}
	parent := ctx
	ctx, cancel := context.WithCancel(parent)
	idle := time.AfterFunc(mailboxWatchIdle, cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/messages/watch?"+params.Encode(), nil)
	if err != nil {
		idle.Stop()
		cancel()
		return nil, nil, err
	}
	longClient := *c.http
	longClient.Timeout = 0
	resp, err := longClient.Do(req)
	if err != nil {
		idle.Stop()
		cancel()
		return nil, nil, wrapIfUnreachable(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		idle.Stop()
		cancel()
		return nil, nil, &MailboxWatchStatusError{Status: resp.StatusCode, Err: readError(resp)}
	}

	events := make(chan MailboxWatchEvent, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errCh)
		defer resp.Body.Close()
		defer cancel()
		defer idle.Stop()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var kind string
		var data strings.Builder
		flush := func() error {
			defer func() { kind = ""; data.Reset() }()
			if kind == "" {
				return nil
			}
			ev := MailboxWatchEvent{Type: kind}
			switch kind {
			case "message":
				if err := json.Unmarshal([]byte(data.String()), &ev.Message); err != nil {
					return fmt.Errorf("decode mailbox message: %w", err)
				}
				ev.Seq = ev.Message.Seq
			case "synchronized", "gap":
				var body struct {
					Seq          int64  `json:"seq"`
					HighWaterSeq int64  `json:"high_water_seq"`
					Reason       string `json:"reason"`
				}
				if err := json.Unmarshal([]byte(data.String()), &body); err != nil {
					return fmt.Errorf("decode mailbox %s: %w", kind, err)
				}
				ev.Seq, ev.Reason = max(body.Seq, body.HighWaterSeq), body.Reason
			default:
				return nil
			}
			select {
			case events <- ev:
			case <-ctx.Done():
			}
			return nil
		}
		for scanner.Scan() {
			idle.Reset(mailboxWatchIdle)
			line := scanner.Text()
			switch {
			case line == "":
				if err := flush(); err != nil {
					errCh <- err
					return
				}
			case strings.HasPrefix(line, "event:"):
				kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if err := scanner.Err(); err != nil && parent.Err() == nil {
			errCh <- fmt.Errorf("read mailbox stream: %w", err)
		}
	}()
	return events, errCh, nil
}
