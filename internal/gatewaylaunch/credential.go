// Package gatewaylaunch owns the opt-in standalone gateway handoff. Provider
// configurations contain a relay command and a path, never bearer contents.
package gatewaylaunch

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hollis-labs/libs/harness/core/agentlaunch"
	"github.com/hollis-labs/libs/security/secrets/tokenfile"
)

const Name = "gateway"

const URLenv = "TETHER_MCP_GATEWAY_URL"
const TokenFileEnv = "TETHER_MCP_GATEWAY_TOKEN_FILE" //nolint:gosec // G101: environment key, never a credential.

// ValidateURL refuses redirects/userinfo and cleartext off loopback. The relay
// separately refuses HTTP redirects when making every request.
func ValidateURL(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid gateway MCP endpoint")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()) {
		return nil
	}
	return errors.New("gateway MCP endpoint requires https or loopback http")
}

func Server(binary, endpoint, path string) agentlaunch.MCPServerSpec {
	return agentlaunch.MCPServerSpec{Name: Name, Command: binary, Args: []string{"mcp-http-relay", "--url", endpoint, "--token-file", path, "--run-token-file", RunTokenPath(path)}}
}

type Credential struct {
	Token     string
	ExpiresAt time.Time
}
type Mint func(context.Context) (Credential, error)

// Start mints before returning and refreshes halfway through each actual lease,
// not an assumed TTL. On failure it retries while the old lease remains valid;
// expired credentials are removed. Stop joins the writer before removing tmpfs.
func Start(mint Mint) (path string, stop func(), err error) {
	dir, err := memoryDir()
	if err != nil {
		return "", nil, err
	}
	path = filepath.Join(dir, "gateway.token")
	return startAt(path, mint)
}

// Resume restarts a worker-owned lease in the same private tmpfs directory.
// Only a path previously returned by Start is accepted; no disk fallback.
func Resume(path string, mint Mint) (func(), error) {
	if err := validateMemoryPath(path); err != nil {
		return nil, err
	}
	_, stop, err := startAt(path, mint)
	return stop, err
}
func startAt(path string, mint Mint) (string, func(), error) {
	dir := filepath.Dir(path)
	first, err := mint(context.Background())
	if err == nil {
		err = write(path, first)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, errors.New("gateway session credential unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		current := first
		delay := time.Until(current.ExpiresAt) / 2
		for {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			next, err := mint(ctx)
			if err == nil {
				err = write(path, next)
			}
			if err == nil {
				current = next
				delay = time.Until(current.ExpiresAt) / 2
				continue
			}
			remaining := time.Until(current.ExpiresAt)
			if remaining <= 0 {
				_ = os.Remove(path)
				delay = time.Second
			} else {
				delay = min(time.Second, remaining)
			}
		}
	}()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done; _ = os.RemoveAll(dir) }) }
	return path, stop, nil
}
func write(path string, c Credential) error {
	if c.Token == "" || time.Until(c.ExpiresAt) <= 0 {
		return errors.New("invalid gateway credential lease")
	}
	return tokenfile.Write(path, c.Token)
}

// BindRunToken preserves launcher authority across CLI stdio environment
// filtering. Only the tmpfs path is planted; the token never enters config.
func BindRunToken(path, token string) error {
	if err := validateMemoryPath(path); err != nil {
		return err
	}
	if token == "" || len(token) > 16<<10 {
		return errors.New("gateway execution token unavailable")
	}
	return tokenfile.Write(RunTokenPath(path), token)
}
func RunTokenPath(path string) string { return filepath.Join(filepath.Dir(path), "run.token") }

// ReadRunToken reads an explicit protected handoff when configured; a direct
// relay invocation may instead use its runtime environment.
func ReadRunToken(path, environment string) (string, error) {
	if path == "" {
		return environment, nil
	}
	token, err := tokenfile.Read(path)
	if err != nil {
		return "", errors.New("gateway execution token unavailable")
	}
	return token, nil
}
