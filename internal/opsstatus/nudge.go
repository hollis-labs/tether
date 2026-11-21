package opsstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/store"
)

// Nudge kinds (convention 5.2, runtime reflexes): at task done without a
// work_done post the agent is reminded once; at blocked it is asked why once.
const (
	NudgeWorkDone = "work_done"
	NudgeBlocked  = "blocked"
)

// TaskReader reads the Torque facts a nudge decision needs.
type TaskReader interface {
	// Task returns the task's status and blocked reason.
	Task(ctx context.Context, id string) (status, blockedReason string, err error)
	// HasStatusComment reports a `STATUS <kind>` comment on the task, which the
	// Ops service writes for every agent-posted kind (convention 5.3).
	HasStatusComment(ctx context.Context, id, kind string) (bool, error)
}

// NudgeStore is the store slice the nudger uses.
type NudgeStore interface {
	ListSessions(opts store.ListSessionsOptions) ([]store.SessionRow, error)
	OpsStatusNudged(ctx context.Context, sessionID, taskID, kind string) (bool, error)
	MarkOpsStatusNudged(ctx context.Context, sessionID, taskID, kind string) error
}

// TurnSender delivers text to a live session as a turn.
type TurnSender func(ctx context.Context, sessionID, text string) error

// Nudger polls running task-bound sessions and reminds each agent once.
type Nudger struct {
	store    NudgeStore
	tasks    TaskReader
	send     TurnSender
	interval time.Duration

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// NewNudger builds a nudger; interval zero means one minute.
func NewNudger(st NudgeStore, tasks TaskReader, send TurnSender, interval time.Duration) *Nudger {
	if interval <= 0 {
		interval = time.Minute
	}
	return &Nudger{store: st, tasks: tasks, send: send, interval: interval}
}

// Start runs the poll loop until Close.
func (n *Nudger) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	n.cancel = cancel
	n.done = make(chan struct{})
	go func() {
		defer close(n.done)
		t := time.NewTicker(n.interval)
		defer t.Stop()
		for {
			n.Tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Close stops the loop and waits for it.
func (n *Nudger) Close() {
	if n == nil || n.cancel == nil {
		return
	}
	n.once.Do(n.cancel)
	<-n.done
}

// Tick runs one pass. Errors are logged; the next tick tries again.
func (n *Nudger) Tick(ctx context.Context) {
	rows, err := n.store.ListSessions(store.ListSessionsOptions{State: "running", Limit: 1000})
	if err != nil {
		slog.Warn("opsstatus: nudge list sessions", "err", err)
		return
	}
	type taskFacts struct {
		status, blocked string
		err             error
	}
	seen := map[string]taskFacts{}
	for _, row := range rows {
		task := row.TaskID.String
		if task == "" || ctx.Err() != nil {
			continue
		}
		f, ok := seen[task]
		if !ok {
			f.status, f.blocked, f.err = n.tasks.Task(ctx, task)
			seen[task] = f
		}
		if f.err != nil {
			continue
		}
		switch f.status {
		case "done":
			n.maybe(ctx, row, task, NudgeWorkDone, workDoneText(task))
		case "blocked":
			if strings.TrimSpace(f.blocked) == "" {
				n.maybe(ctx, row, task, NudgeBlocked, blockedText(task))
			}
		}
	}
}

func (n *Nudger) maybe(ctx context.Context, row store.SessionRow, task, kind, text string) {
	done, err := n.store.OpsStatusNudged(ctx, row.ID, task, kind)
	if err != nil || done {
		return
	}
	posted, err := n.tasks.HasStatusComment(ctx, task, kind)
	if err != nil {
		return
	}
	if posted {
		// The agent already did it; never remind.
		_ = n.store.MarkOpsStatusNudged(ctx, row.ID, task, kind)
		return
	}
	if err := n.send(ctx, row.ID, text); err != nil {
		slog.Warn("opsstatus: nudge send", "session", row.ID, "task", task, "kind", kind, "err", err)
		return
	}
	if err := n.store.MarkOpsStatusNudged(ctx, row.ID, task, kind); err != nil {
		slog.Warn("opsstatus: mark nudge", "session", row.ID, "task", task, "kind", kind, "err", err)
	}
	slog.Info("opsstatus: nudged", "session", row.ID, "task", task, "kind", kind)
}

func workDoneText(task string) string {
	return fmt.Sprintf("[tether] %s is done in Torque, but there is no work_done status post for it. "+
		"Post one now with ops_status_post (state done, kind work_done, task %s, a one-line outcome, and evidence: PRs, docs, deploys). "+
		"This reminder is sent once.", task, task)
}

func blockedText(task string) string {
	return fmt.Sprintf("[tether] %s is blocked in Torque with no reason recorded. "+
		"Post why with ops_status_post (state blocked, kind blocked, task %s, summary saying on what, awaiting the handle you need). "+
		"This reminder is sent once.", task, task)
}

// TorqueClient reads tasks and comments over Torque's HTTP API.
type TorqueClient struct {
	BaseURL   string
	TokenFile string
	Token     string
	HTTP      *http.Client
}

// TorqueFromEnvironment uses the daemon's TORQUE_BASE_URL and its Torque credential.
func TorqueFromEnvironment() *TorqueClient {
	base := strings.TrimSpace(os.Getenv("TORQUE_BASE_URL"))
	if base == "" {
		base = "http://127.0.0.1:8990"
	}
	return &TorqueClient{BaseURL: strings.TrimRight(base, "/"), TokenFile: os.Getenv("HOLLIS_AUTH_TOKEN_FILE_TORQUE"), Token: os.Getenv("TORQUE_API_TOKEN")}
}

func (c *TorqueClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	token := c.Token
	if c.TokenFile != "" {
		if raw, err := os.ReadFile(c.TokenFile); err == nil {
			token = strings.TrimSpace(string(raw))
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("torque %s: http %d", path, resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

// Task implements TaskReader.
func (c *TorqueClient) Task(ctx context.Context, id string) (string, string, error) {
	var t struct {
		Status        string `json:"status"`
		BlockedReason string `json:"blocked_reason"`
	}
	err := c.get(ctx, "/api/v1/tasks/"+url.PathEscape(id), &t)
	return t.Status, t.BlockedReason, err
}

// HasStatusComment implements TaskReader.
func (c *TorqueClient) HasStatusComment(ctx context.Context, id, kind string) (bool, error) {
	var page struct {
		Items []struct {
			Content string `json:"content"`
		} `json:"items"`
	}
	if err := c.get(ctx, "/api/v1/tasks/"+url.PathEscape(id)+"/comments?limit=200", &page); err != nil {
		return false, err
	}
	prefix := "STATUS " + kind
	for _, it := range page.Items {
		if strings.HasPrefix(strings.TrimSpace(it.Content), prefix) {
			return true, nil
		}
	}
	return false, nil
}
