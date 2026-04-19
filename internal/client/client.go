// Package client is the thin HTTP client used by the mux CLI to talk to a
// running muxd daemon. It wraps the transport helpers in internal/daemon
// and the JSON payload types defined in internal/api, so CLI subcommands
// stay free of HTTP plumbing.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/config"
	"github.com/chrispian/agent-mux/internal/daemon"
)

// ErrDaemonUnreachable means the daemon process is not accepting
// connections (socket missing, connection refused, or dial timeout). CLI
// callers inspect this to decide whether to fall back to a read-only
// local path (e.g., `sessions list` hitting SQLite directly).
var ErrDaemonUnreachable = errors.New("daemon unreachable")

// Client talks to a muxd daemon listening at ListenAddr.
type Client struct {
	baseURL string
	http    *http.Client
}

// New constructs a Client for the given listen_addr. The transport handles
// unix: and tcp: schemes the same way daemon.Server accepts them.
func New(listenAddr string) *Client {
	return &Client{
		baseURL: daemon.BaseURL(listenAddr),
		http:    daemon.DialHTTPClient(listenAddr),
	}
}

// Ping issues a short GET /health to verify the daemon is reachable. It
// returns ErrDaemonUnreachable for connection-level failures so callers
// can branch on it with errors.Is.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if isUnreachable(err) {
			return fmt.Errorf("%w: %w", ErrDaemonUnreachable, err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health status %d", resp.StatusCode)
	}
	return nil
}

// Health fetches the daemon's /health payload. Returns ErrDaemonUnreachable
// on connection failure.
func (c *Client) Health(ctx context.Context) (daemon.Health, error) {
	var h daemon.Health
	if err := c.getJSON(ctx, "/health", &h); err != nil {
		return daemon.Health{}, err
	}
	return h, nil
}

// CreateSession POSTs /sessions and returns the created session's
// metadata. The session is in state=created; call LaunchSession to
// start it.
func (c *Client) CreateSession(ctx context.Context, launchID string) (api.LaunchResponse, error) {
	body, _ := json.Marshal(api.LaunchRequest{Launch: launchID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sessions", bytes.NewReader(body))
	if err != nil {
		return api.LaunchResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return api.LaunchResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return api.LaunchResponse{}, readError(resp)
	}
	var res api.LaunchResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return api.LaunchResponse{}, fmt.Errorf("decode create response: %w", err)
	}
	return res, nil
}

// LaunchSession POSTs /sessions/{id}/launch to transition a created
// session to running. Returns the launched metadata.
func (c *Client) LaunchSession(ctx context.Context, sessionID string) (api.LaunchResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/sessions/"+url.PathEscape(sessionID)+"/launch", nil)
	if err != nil {
		return api.LaunchResponse{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return api.LaunchResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.LaunchResponse{}, readError(resp)
	}
	var res api.LaunchResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return api.LaunchResponse{}, fmt.Errorf("decode launch response: %w", err)
	}
	return res, nil
}

// Launch is a convenience that creates then launches, returning the
// final response. The common case — CLI callers want one round-trip-
// per-user-action, not two.
func (c *Client) Launch(ctx context.Context, launchID string) (api.LaunchResponse, error) {
	created, err := c.CreateSession(ctx, launchID)
	if err != nil {
		return api.LaunchResponse{}, err
	}
	return c.LaunchSession(ctx, created.ID)
}

// ListOptions narrows a ListSessions request. Fields map 1:1 onto the
// GET /sessions query params: ?limit=, ?cursor=, ?state=. A zero-valued
// ListOptions retrieves the first default-size page with no filter.
type ListOptions struct {
	Limit  int
	Cursor string
	State  string
}

// ListResult carries the session page plus any pagination cursor the
// server emitted. NextCursor is empty when the caller has reached the
// end of the range.
type ListResult struct {
	Sessions   []api.SessionDTO
	NextCursor string
}

func (c *Client) ListSessions(ctx context.Context, opts ListOptions) (*ListResult, error) {
	u := "/sessions"
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.State != "" {
		q.Set("state", opts.State)
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var res api.ListSessionsResponse
	if err := c.getJSON(ctx, u, &res); err != nil {
		return nil, err
	}
	return &ListResult{Sessions: res.Sessions, NextCursor: res.NextCursor}, nil
}

// GetSession fetches one session by id. Returns os-style not-found semantics
// via a descriptive error when the daemon returns 404.
func (c *Client) GetSession(ctx context.Context, id string) (api.SessionDTO, error) {
	var dto api.SessionDTO
	if err := c.getJSON(ctx, "/sessions/"+url.PathEscape(id), &dto); err != nil {
		return api.SessionDTO{}, err
	}
	return dto, nil
}

// StopSession signals the daemon to terminate a running session. Returns
// nil on 204. A 404 (session not running) is surfaced as an error.
func (c *Client) StopSession(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sessions/"+url.PathEscape(id)+"/stop", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return readError(resp)
}

// SendInput writes data to the named session's PTY via the daemon's input
// endpoint. The body is sent raw (application/octet-stream); no framing
// or newline handling — the caller decides.
func (c *Client) SendInput(ctx context.Context, id string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/sessions/"+url.PathEscape(id)+"/input", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return readError(resp)
}

// AttachSession opens a streaming GET and copies live PTY output to w until
// ctx is canceled or the session exits (server closes the response).
// Returns nil on clean EOF (session terminated); returns a wrapped ctx.Err
// when the caller cancels.
//
// sinceSeq is a byte-offset resume hint. 0 requests the full replay
// ring (pre-resume default). Nonzero asks the server to replay only
// bytes beyond that offset — useful for reconnect after a detach
// where the client already has a partial byte-count watermark.
func (c *Client) AttachSession(ctx context.Context, id string, w io.Writer, sinceSeq int64) error {
	// Drop the 5s transport timeout — attach is long-lived.
	longClient := *c.http
	longClient.Timeout = 0
	path := c.baseURL + "/sessions/" + url.PathEscape(id) + "/attach"
	if sinceSeq > 0 {
		path += "?since_seq=" + strconv.FormatInt(sinceSeq, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := longClient.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	_, err = io.Copy(w, resp.Body)
	// ctx cancellation turns into a ctx error at the copy layer; surface it.
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return err
	}
	return nil
}

// WaitSession long-polls the daemon until the session terminates; returns
// the exit code recorded by the runtime manager. The ctx controls the
// client-side timeout; the daemon itself does not apply one.
func (c *Client) WaitSession(ctx context.Context, id string) (int, error) {
	var res api.WaitResponse
	// Override the default 5s transport timeout for this specific call so
	// long-running sessions can block.
	longClient := *c.http
	longClient.Timeout = 0
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/sessions/"+url.PathEscape(id)+"/wait", nil)
	if err != nil {
		return 0, err
	}
	resp, err := longClient.Do(req)
	if err != nil {
		return 0, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, readError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return 0, fmt.Errorf("decode wait response: %w", err)
	}
	return res.ExitCode, nil
}

// ListProjects fetches the full project catalog via GET /catalog/projects.
// No filtering or pagination — catalog sizes are small. Callers filter
// locally. Returns ErrDaemonUnreachable on connection failure.
func (c *Client) ListProjects(ctx context.Context) ([]config.Project, error) {
	var res api.ListProjectsResponse
	if err := c.getJSON(ctx, "/catalog/projects", &res); err != nil {
		return nil, err
	}
	return res.Projects, nil
}

// ListAgents fetches the full agent catalog via GET /catalog/agents.
func (c *Client) ListAgents(ctx context.Context) ([]config.Agent, error) {
	var res api.ListAgentsResponse
	if err := c.getJSON(ctx, "/catalog/agents", &res); err != nil {
		return nil, err
	}
	return res.Agents, nil
}

// ListProviders fetches the full provider catalog via GET /catalog/providers.
func (c *Client) ListProviders(ctx context.Context) ([]config.Provider, error) {
	var res api.ListProvidersResponse
	if err := c.getJSON(ctx, "/catalog/providers", &res); err != nil {
		return nil, err
	}
	return res.Providers, nil
}

// ListLaunches fetches the full launch catalog via GET /catalog/launches.
func (c *Client) ListLaunches(ctx context.Context) ([]config.Launch, error) {
	var res api.ListLaunchesResponse
	if err := c.getJSON(ctx, "/catalog/launches", &res); err != nil {
		return nil, err
	}
	return res.Launches, nil
}

// getJSON is a small helper for the GET + decode + error flow shared by
// Health / ListSessions / GetSession.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// readError reads a daemon error envelope from resp.Body. Returns a
// descriptive error that preserves the HTTP status + error code for
// callers that want to branch on 404 vs. 500 or on the typed code.
func readError(resp *http.Response) error {
	var env api.ErrorResponse
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Message != "" {
		return fmt.Errorf("daemon %d (%s): %s", resp.StatusCode, env.Error.Code, env.Error.Message)
	}
	return fmt.Errorf("daemon %d: %s", resp.StatusCode, string(body))
}

// wrapIfUnreachable annotates connection-refused / socket-missing errors
// so CLI callers can fall back to a local path.
func wrapIfUnreachable(err error) error {
	if isUnreachable(err) {
		return fmt.Errorf("%w: %w", ErrDaemonUnreachable, err)
	}
	return err
}

func isUnreachable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Dial errors against a non-existent socket or stopped TCP listener
		// land here with opErr.Op == "dial".
		if opErr.Op == "dial" {
			return true
		}
	}
	// Fall back to substring sniff for wrapped forms.
	s := err.Error()
	return strings.Contains(s, "connection refused") || strings.Contains(s, "no such file or directory")
}
