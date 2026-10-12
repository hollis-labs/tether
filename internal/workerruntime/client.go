// Package workerruntime implements the thin execution outpost. It opens no
// Tether catalog, session DB, directory or messaging store.
package workerruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/hollis-labs/tether/internal/devicepair"
	"github.com/hollis-labs/tether/internal/environment/report"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/workerwire"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Hub      string `json:"hub"`
	Label    string `json:"label"`
	WorkerID string `json:"worker_id,omitempty"`
}

func SaveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".worker-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}
func Load(root string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(filepath.Join(root, "config.json")) //nolint:gosec // G304: root is the worker's own state directory
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(b, &cfg)
	return cfg, err
}
func Enroll(ctx context.Context, root, hub, code, label string) error {
	u, err := url.Parse(hub)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("hub must be an HTTP(S) URL")
	}
	if label == "" {
		return fmt.Errorf("worker label required")
	}
	result, err := devicepair.Exchange(ctx, &http.Client{Timeout: 30 * time.Second}, hub, code)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(root, "device.token"), []byte(result.Token+"\n"), 0600); err != nil {
		return err
	}
	if err = SaveJSON(filepath.Join(root, "config.json"), Config{Hub: strings.TrimRight(hub, "/"), Label: label}); err != nil {
		return err
	}
	path := filepath.Join(root, "hub-mcp.json")
	if _, err = os.Stat(path); os.IsNotExist(err) {
		return SaveJSON(path, map[string]any{"mcpServers": map[string]any{}})
	}
	return nil
}

// WorkerLaunchTimeout bounds one launch on the worker. It must stay below the hub's launch wait
// (internal/app workerLaunchWait) so the worker aborts a slow launch before the hub marks it failed.
const WorkerLaunchTimeout = 4 * time.Minute

type Worker struct {
	Root      string
	Config    Config
	Version   string
	mu        sync.Mutex
	ws        *websocket.Conn
	ready     bool
	write     sync.Mutex
	Caps      func(context.Context) json.RawMessage
	Inventory func() []workerwire.Session
	Command   func(context.Context, workerwire.Frame) error
	OnConnect func()
}

func (w *Worker) capabilities(ctx context.Context) json.RawMessage {
	if w.Caps != nil {
		return w.Caps(ctx)
	}
	d := report.DefaultDetectors()
	r := report.Report{Providers: d.DetectProviders(ctx, report.SandboxProtectData{}), Hosting: d.DetectHosting(ctx, true), Filesystem: d.DetectFilesystem(ctx, w.Root), Resources: d.MeasureResources(w.Root, w.Root), RoleProfile: report.RoleProfile{Role: "outpost"}}
	b, _ := json.Marshal(r)
	return b
}
func (w *Worker) Send(f workerwire.Frame) error {
	w.write.Lock()
	defer w.write.Unlock()
	w.mu.Lock()
	ws := w.ws
	ready := w.ready
	w.mu.Unlock()
	if ws == nil || (!ready && f.T != "hello") {
		return fmt.Errorf("hub disconnected")
	}
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ws.WriteJSON(f)
}
func (w *Worker) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return nil //nolint:nilerr // context cancellation is the clean shutdown path, not a failure
		}
		err := w.connect(ctx)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // shutdown during connect is a clean exit; err is only the canceled dial
		}
		if err != nil {
			log.Printf("worker channel: %v; reconnect in %s", err, backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(15*time.Second, backoff*2)
	}
}
func (w *Worker) connect(ctx context.Context) error {
	token, err := identity.ReadTokenFile(filepath.Join(w.Root, "device.token"))
	if err != nil {
		return err
	}
	u, err := url.Parse(w.Config.Hub)
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + workerwire.ConnectPath
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set("Tether-Protocol", "1")
	ws, res, err := websocket.DefaultDialer.DialContext(ctx, u.String(), header)
	if res != nil && res.Body != nil {
		_ = res.Body.Close()
	}
	if err != nil {
		return err
	}
	defer func() { _ = ws.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	w.mu.Lock()
	w.ws = ws
	w.ready = false
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.ws == ws {
			w.ws = nil
			w.ready = false
		}
		w.mu.Unlock()
	}()
	var inventory []workerwire.Session
	if w.Inventory != nil {
		inventory = w.Inventory()
	}
	if err = w.Send(workerwire.Frame{T: "hello", Proto: 1, Worker: &workerwire.Worker{Label: w.Config.Label, Version: w.Version, Platform: runtime.GOOS, Arch: runtime.GOARCH}, Caps: w.capabilities(ctx), Sessions: inventory}); err != nil {
		return err
	}
	_ = ws.SetReadDeadline(time.Now().Add(45 * time.Second))
	for {
		var f workerwire.Frame
		if err = ws.ReadJSON(&f); err != nil {
			return err
		}
		_ = ws.SetReadDeadline(time.Now().Add(45 * time.Second))
		switch f.T {
		case "welcome":
			w.mu.Lock()
			w.ready = true
			w.mu.Unlock()
			w.Config.WorkerID = f.WorkerID
			_ = SaveJSON(filepath.Join(w.Root, "config.json"), w.Config)
			if w.OnConnect != nil {
				w.OnConnect()
			}
			if w.Command != nil {
				if err := w.Command(ctx, f); err != nil {
					log.Printf("worker welcome: %v", err)
				}
			}
		case "ping":
			if err = w.Send(workerwire.Frame{T: "pong", TS: f.TS}); err != nil {
				return err
			}
		default:
			if w.Command != nil {
				run := func(c context.Context) {
					if err := w.Command(c, f); err != nil {
						_ = w.Send(workerwire.Frame{T: "launch_failed", CmdID: f.CmdID, Session: f.Session, Error: err.Error()})
					}
				}
				if f.T == "launch" {
					// A launch clones a repository and can take minutes. Run it off the read loop so pings,
					// stops and input keep flowing, and bound it below the hub's wait so a launch the hub
					// has given up on is aborted here instead of starting an untracked agent.
					go func() {
						lctx, cancel := context.WithTimeout(ctx, WorkerLaunchTimeout)
						defer cancel()
						run(lctx)
					}()
				} else {
					run(ctx)
				}
			} else {
				_ = w.Send(workerwire.Frame{T: "launch_failed", CmdID: f.CmdID, Session: f.Session, Error: "execution not installed"})
			}
		}
	}
}
