package opsstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Ops service operator-token headers (shared-services/ops README, "Operator-token callers").
const (
	headerActor = "X-Actor"
	headerRun   = "X-Run-Id"
	headerTask  = "X-Task-Id"
)

// Client posts to the Ops service MCP endpoint as a stateless tools/call,
// authenticated with the host operator token and a stated actor.
type Client struct {
	URL       string
	TokenFile string
	Actor     string
	// Tool is the Ops verb name; default "status_post" (the gateway exposes it as ops_status_post).
	Tool string
	HTTP *http.Client

	id atomic.Int64
}

// Post implements Poster.
func (c *Client) Post(ctx context.Context, p Post) error {
	token, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return &RetryableError{fmt.Errorf("read token: %w", err)}
	}
	tool := c.Tool
	if tool == "" {
		tool = "status_post"
	}
	args, err := json.Marshal(p)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      c.id.Add(1),
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": json.RawMessage(args)},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Actor != "" {
		req.Header.Set(headerActor, c.Actor)
	}
	if p.Task != "" {
		req.Header.Set(headerTask, p.Task)
	}
	if p.RunID != "" {
		req.Header.Set(headerRun, p.RunID)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return &RetryableError{err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusUnauthorized {
		return &RetryableError{fmt.Errorf("ops: http %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ops: http %d: %s", resp.StatusCode, snippet(raw))
	}
	return parseResult(raw)
}

// parseResult reads a JSON-RPC tools/call response. A tool error with a code
// that can clear on its own (forbidden until the service accepts Tether's
// posts, unavailable backends) is retryable; anything else is a refusal.
func parseResult(raw []byte) error {
	if s := bytes.TrimSpace(raw); bytes.HasPrefix(s, []byte("event:")) || bytes.HasPrefix(s, []byte("data:")) {
		for _, line := range strings.Split(string(s), "\n") {
			if strings.HasPrefix(line, "data:") {
				raw = []byte(strings.TrimSpace(line[5:]))
				break
			}
		}
	}
	var rpc struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			IsError           bool `json:"isError"`
			StructuredContent struct {
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			} `json:"structuredContent"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &rpc); err != nil {
		return &RetryableError{fmt.Errorf("ops: unreadable response: %w", err)}
	}
	if rpc.Error != nil {
		// Unknown tool or bad params come back as JSON-RPC errors; the service
		// may not have the extended verb yet, so hold.
		return &RetryableError{fmt.Errorf("ops: rpc %d: %s", rpc.Error.Code, rpc.Error.Message)}
	}
	if rpc.Result == nil {
		return &RetryableError{errors.New("ops: empty result")}
	}
	if !rpc.Result.IsError {
		return nil
	}
	code, msg := "", ""
	if e := rpc.Result.StructuredContent.Error; e != nil {
		code, msg = e.Code, e.Message
	} else if len(rpc.Result.Content) > 0 {
		msg = rpc.Result.Content[0].Text
	}
	err := fmt.Errorf("ops: %s: %s", code, msg)
	switch code {
	case "forbidden", "unauthorized", "unavailable", "timeout", "rate_limited", "":
		// forbidden is what the service answers operator-token callers until it
		// accepts Tether's posts (convention 5.3); holding keeps those events
		// for the cutover. MaxAge bounds the hold. validation is a real refusal
		// and is skipped, so one bad post cannot stall the stream.
		return &RetryableError{err}
	}
	return err
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
