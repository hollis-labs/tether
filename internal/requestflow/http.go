package requestflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// HTTPBackend uses a daemon service credential. TokenFile is re-read per call
// so rotation does not need a daemon restart. Errors never include credentials
// or upstream response bodies.
type HTTPBackend struct {
	BaseURL   string
	TokenFile string
	Token     string
	Client    *http.Client
}

func FromEnvironment() Backend {
	base := strings.TrimSpace(os.Getenv("TORQUE_BASE_URL"))
	if base == "" {
		return nil
	}
	return &HTTPBackend{BaseURL: strings.TrimRight(base, "/"), TokenFile: os.Getenv("HOLLIS_AUTH_TOKEN_FILE_TORQUE"), Token: os.Getenv("TORQUE_API_TOKEN"), Client: &http.Client{Timeout: 10 * time.Second}}
}

func (h *HTTPBackend) call(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(h.BaseURL, "/")+path, body)
	if err != nil {
		return fmt.Errorf("invalid Torque request URL")
	}
	req.Header.Set("Content-Type", "application/json")
	token := h.Token
	if h.TokenFile != "" {
		b, err := os.ReadFile(h.TokenFile)
		if err != nil {
			return fmt.Errorf("read Torque credential file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("torque request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("torque %s %s returned HTTP %d", method, path, resp.StatusCode)
	}
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode Torque response: %w", err)
	}
	return nil
}

func (h *HTTPBackend) Submit(ctx context.Context, in Submit) (Request, error) {
	var out Request
	err := h.call(ctx, http.MethodPost, "/api/v1/requests/submit", in, &out)
	return out, err
}
func (h *HTTPBackend) AttachMessage(ctx context.Context, id, messageID string) error {
	return h.call(ctx, http.MethodPut, "/api/v1/tasks/"+url.PathEscape(id)+"/request/mail", map[string]string{"mail_message_id": messageID}, nil)
}
func (h *HTTPBackend) Transition(ctx context.Context, id string, in Transition) error {
	summary := in.Summary
	if len(in.Evidence) > 0 {
		summary += "\n\nEvidence:\n- " + strings.Join(in.Evidence, "\n- ")
	}
	body := map[string]string{"state": in.State, "summary": summary, "author": in.ActorURN}
	return h.call(ctx, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(id)+"/request/transition", body, nil)
}
func (h *HTTPBackend) DueAlerts(ctx context.Context) ([]Request, error) {
	cutoff := time.Now().UTC().Format(time.RFC3339Nano)
	cursor := ""
	var rows []Request
	for {
		var out struct {
			Items []Request `json:"items"`
			Next  string    `json:"next_after_task_id"`
		}
		path := "/api/v1/requests?awaiting_ack=true&due_before=" + url.QueryEscape(cutoff) + "&limit=100&after_task_id=" + url.QueryEscape(cursor)
		if err := h.call(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		rows = append(rows, out.Items...)
		if out.Next == "" {
			return rows, nil
		}
		if out.Next <= cursor {
			return nil, fmt.Errorf("torque request cursor did not advance")
		}
		cursor = out.Next
	}
}
func (h *HTTPBackend) MarkAlerted(ctx context.Context, id string) error {
	return h.Transition(ctx, id, Transition{State: "unacknowledged_alert"})
}
