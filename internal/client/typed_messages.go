package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Typed messages and seat status (CW-20261114-0013). The daemon validates
// everything; these methods only carry the JSON.

// MessagePostResult is the POST /messages/post response.
type MessagePostResult struct {
	IDs      []string        `json:"ids"`
	ThreadID string          `json:"thread_id"`
	Envelope json.RawMessage `json:"envelope"`
}

func withAs(path, as string) string {
	if as == "" {
		return path
	}
	return path + "?as=" + url.QueryEscape(as)
}

// MessagePost sends a typed envelope (raw envelope JSON) as as; empty as means
// the authenticated caller.
func (c *Client) MessagePost(ctx context.Context, as, urgency string, envelopeJSON []byte, key string) (MessagePostResult, error) {
	var out MessagePostResult
	q := url.Values{}
	if as != "" {
		q.Set("as", as)
	}
	if urgency != "" {
		q.Set("urgency", urgency)
	}
	path := "/messages/post"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	if err := c.postKeyed(ctx, path, envelopeJSON, key, &out, "typed message"); err != nil {
		return MessagePostResult{}, err
	}
	return out, nil
}

// PendingReplies lists inbound typed messages that wait on as.
func (c *Client) PendingReplies(ctx context.Context, as string) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.getJSON(ctx, withAs("/messages/pending-replies", as), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// StatusSet declares a seat's status (body is the POST /status request JSON).
func (c *Client) StatusSet(ctx context.Context, as string, body []byte) (json.RawMessage, error) {
	return c.postOK(ctx, withAs("/status", as), body)
}

// StatusGet returns a seat's status card.
func (c *Client) StatusGet(ctx context.Context, handle string) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.getJSON(ctx, "/status/"+url.PathEscape(handle), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// HandleSet gives a registry agent a @handle (operator only).
func (c *Client) HandleSet(ctx context.Context, urn, handle string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]string{"urn": urn, "handle": handle})
	if err != nil {
		return nil, err
	}
	return c.postOK(ctx, "/registry/handles", body)
}

func (c *Client) postOK(ctx context.Context, path string, body []byte) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readError(resp)
	}
	var out json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", path, err)
	}
	return out, nil
}

// StopGateReport tells the hub the seat's Stop gate blocked a turn on these
// messages (it publishes seat.gate_blocked).
func (c *Client) StopGateReport(ctx context.Context, as string, messageIDs []string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"message_ids": messageIDs})
	if err != nil {
		return nil, err
	}
	return c.postOK(ctx, withAs("/messages/pending-replies/blocked", as), body)
}
