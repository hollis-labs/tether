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
)

type EventsStreamQuery struct {
	SinceSeq  int64
	Scopes    []string
	Kinds     []string
	SessionID string
}

type StreamEvent struct {
	Seq         int64
	Kind        string
	Scope       string
	SessionID   string
	PayloadJSON string
}

func (c *Client) StreamEvents(ctx context.Context, q EventsStreamQuery) (<-chan StreamEvent, <-chan error, error) {
	path := "/events/stream"
	params := url.Values{}
	if q.SinceSeq > 0 {
		params.Set("since_seq", strconv.FormatInt(q.SinceSeq, 10))
	}
	for _, scope := range q.Scopes {
		if strings.TrimSpace(scope) != "" {
			params.Add("scope", scope)
		}
	}
	for _, kind := range q.Kinds {
		if strings.TrimSpace(kind) != "" {
			params.Add("kind", kind)
		}
	}
	if q.SessionID != "" {
		params.Set("session_id", q.SessionID)
	}
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, wrapIfUnreachable(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, nil, readError(resp)
	}

	eventsCh := make(chan StreamEvent, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(eventsCh)
		defer close(errCh)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var seq int64
		var kind string
		var data strings.Builder
		flush := func() error {
			if kind == "" && seq == 0 && data.Len() == 0 {
				return nil
			}
			var payload struct {
				Scope       string `json:"scope"`
				SessionID   string `json:"session_id,omitempty"`
				PayloadJSON string `json:"payload_json,omitempty"`
			}
			if data.Len() > 0 {
				if err := json.Unmarshal([]byte(data.String()), &payload); err != nil {
					return fmt.Errorf("decode event stream payload: %w", err)
				}
			}
			select {
			case eventsCh <- StreamEvent{
				Seq:         seq,
				Kind:        kind,
				Scope:       payload.Scope,
				SessionID:   payload.SessionID,
				PayloadJSON: payload.PayloadJSON,
			}:
			case <-ctx.Done():
				return nil
			}
			seq = 0
			kind = ""
			data.Reset()
			return nil
		}

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if err := flush(); err != nil {
					errCh <- err
					return
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			switch {
			case strings.HasPrefix(line, "id:"):
				v := strings.TrimSpace(strings.TrimPrefix(line, "id:"))
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					seq = n
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
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("read event stream: %w", err)
			return
		}
		if err := flush(); err != nil && ctx.Err() == nil {
			errCh <- err
		}
	}()
	return eventsCh, errCh, nil
}
