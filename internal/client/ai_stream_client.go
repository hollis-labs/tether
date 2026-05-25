package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/llm"
)

func (c *Client) AIChatStream(ctx context.Context, req api.ChatRequest) (<-chan llm.StreamEvent, <-chan error, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal ai chat stream request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ai/chat/stream", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, nil, wrapIfUnreachable(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, nil, readError(resp)
	}

	eventsCh := make(chan llm.StreamEvent, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(eventsCh)
		defer close(errCh)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var data strings.Builder
		flush := func() error {
			if data.Len() == 0 {
				return nil
			}
			var ev llm.StreamEvent
			if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
				return fmt.Errorf("decode ai chat stream event: %w", err)
			}
			select {
			case eventsCh <- ev:
			case <-ctx.Done():
				return nil
			}
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
			if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
				continue
			}
			if strings.HasPrefix(line, "data:") {
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("read ai chat stream: %w", err)
			return
		}
		if err := flush(); err != nil && ctx.Err() == nil {
			errCh <- err
		}
	}()
	return eventsCh, errCh, nil
}
