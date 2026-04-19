// Package client is the thin HTTP client used by the mux CLI to talk to a
// running muxd daemon. It wraps the transport helpers in internal/daemon
// and the JSON payload types defined there, so CLI subcommands stay free
// of HTTP plumbing.
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
	"strings"
	"syscall"

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
			return fmt.Errorf("%w: %v", ErrDaemonUnreachable, err)
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

// Launch starts a new session on the daemon.
func (c *Client) Launch(ctx context.Context, launchID string) (daemon.LaunchResponse, error) {
	body, _ := json.Marshal(daemon.LaunchRequest{Launch: launchID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sessions", bytes.NewReader(body))
	if err != nil {
		return daemon.LaunchResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return daemon.LaunchResponse{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return daemon.LaunchResponse{}, readError(resp)
	}
	var res daemon.LaunchResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return daemon.LaunchResponse{}, fmt.Errorf("decode launch response: %w", err)
	}
	return res, nil
}

// ListSessions returns the daemon's current view of all sessions (running +
// terminal), flattened into DTOs.
func (c *Client) ListSessions(ctx context.Context) ([]daemon.SessionDTO, error) {
	var res daemon.ListSessionsResponse
	if err := c.getJSON(ctx, "/sessions", &res); err != nil {
		return nil, err
	}
	return res.Sessions, nil
}

// GetSession fetches one session by id. Returns os-style not-found semantics
// via a descriptive error when the daemon returns 404.
func (c *Client) GetSession(ctx context.Context, id string) (daemon.SessionDTO, error) {
	var dto daemon.SessionDTO
	if err := c.getJSON(ctx, "/sessions/"+url.PathEscape(id), &dto); err != nil {
		return daemon.SessionDTO{}, err
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
// ctx is cancelled or the session exits (server closes the response).
// Returns nil on clean EOF (session terminated); returns a wrapped ctx.Err
// when the caller cancels.
func (c *Client) AttachSession(ctx context.Context, id string, w io.Writer) error {
	// Drop the 5s transport timeout — attach is long-lived.
	longClient := *c.http
	longClient.Timeout = 0
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/sessions/"+url.PathEscape(id)+"/attach", nil)
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
	var res daemon.WaitResponse
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
// descriptive error that preserves the HTTP status for callers that
// want to branch on 404 vs. 500.
func readError(resp *http.Response) error {
	var env daemon.ErrorResponse
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &env); err == nil && env.Error != "" {
		return fmt.Errorf("daemon %d: %s", resp.StatusCode, env.Error)
	}
	return fmt.Errorf("daemon %d: %s", resp.StatusCode, string(body))
}

// wrapIfUnreachable annotates connection-refused / socket-missing errors
// so CLI callers can fall back to a local path.
func wrapIfUnreachable(err error) error {
	if isUnreachable(err) {
		return fmt.Errorf("%w: %v", ErrDaemonUnreachable, err)
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
