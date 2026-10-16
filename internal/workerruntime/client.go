// Package workerruntime implements the thin execution outpost. It opens no
// Tether catalog, session DB, directory or messaging store.
package workerruntime

import (
	"context"
	"encoding/json"
	"errors"
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
	MailSocket string `json:"mail_socket,omitempty"`
	MailGID    *int   `json:"mail_gid,omitempty"`
	MailUIDs   []int  `json:"mail_uids,omitempty"`
	Hub        string `json:"hub"`
	Label      string `json:"label"`
	WorkerID   string `json:"worker_id,omitempty"`
}

var ErrHubDisconnected = errors.New("hub disconnected")

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
	Hosts     func() []workerwire.Host
	Command   func(context.Context, workerwire.Frame) error
	OnConnect func()
	Bindings  func() []workerwire.Binding
	pendingMu sync.Mutex
	pending   map[string]chan workerwire.Frame
	commands  chan workerwire.Frame
	launches  chan struct{}
	// Now and Rand are test seams for the reconnect schedule; nil means time.Now and math/rand.
	Now  func() time.Time
	Rand func() float64
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
		return ErrHubDisconnected
	}
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ws.WriteJSON(f)
}
func (w *Worker) Run(ctx context.Context) error {
	if err := ValidateHubMCPFile(w.Root); err != nil {
		log.Printf("WARN worker: launches will fail until this is fixed: %v", err)
	}
	w.pendingMu.Lock()
	if w.pending == nil {
		w.pending = map[string]chan workerwire.Frame{}
	}
	w.pendingMu.Unlock()
	// Keep the socket reader free for ping/replies while a launch clones a repo.
	w.commands = make(chan workerwire.Frame, 32)
	w.launches = make(chan struct{}, 4)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-w.commands:
				if w.Command == nil {
					continue
				}
				if err := w.Command(ctx, f); err != nil {
					_ = w.Send(workerwire.Frame{T: "launch_failed", CmdID: f.CmdID, Session: f.Session, Error: err.Error()})
				}
			}
		}
	}()
	bo := newReconnectBackoff(w.Now, w.Rand)
	for {
		if err := ctx.Err(); err != nil {
			return nil //nolint:nilerr // context cancellation is the clean shutdown path, not a failure
		}
		var upSince time.Time
		err := w.connect(ctx, func() { upSince = bo.now() })
		if ctx.Err() != nil {
			return nil //nolint:nilerr // shutdown during connect is a clean exit; err is only the canceled dial
		}
		if !upSince.IsZero() {
			bo.connectionEnded(bo.now().Sub(upSince))
		}
		delay := bo.next()
		if err != nil {
			log.Printf("worker channel: %v; reconnect in %s", err, delay)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}
func (w *Worker) connect(ctx context.Context, welcomed func()) error {
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
	var hosts []workerwire.Host
	if w.Hosts != nil {
		hosts = w.Hosts()
	}
	var bindings []workerwire.Binding
	if w.Bindings != nil {
		bindings = w.Bindings()
	}
	if err = w.Send(workerwire.Frame{T: "hello", Proto: 1, Features: []string{"bind", "deliver", workerwire.FeatureSessionCred, workerwire.FeatureExtractRefs, workerwire.FeatureEditableAgents, workerwire.FeatureAgentToken, workerwire.FeatureWorkerResume}, Bindings: bindings, Worker: &workerwire.Worker{Label: w.Config.Label, Version: w.Version, Platform: runtime.GOOS, Arch: runtime.GOARCH}, Caps: w.capabilities(ctx), Sessions: inventory, Hosts: hosts}); err != nil {
		return err
	}
	_ = ws.SetReadDeadline(time.Now().Add(45 * time.Second))
	for {
		var f workerwire.Frame
		if err = ws.ReadJSON(&f); err != nil {
			return err
		}
		_ = ws.SetReadDeadline(time.Now().Add(45 * time.Second))
		if f.T == "bind_ack" || f.T == "unbind_ack" {
			w.pendingMu.Lock()
			ch := w.pending[f.CmdID]
			w.pendingMu.Unlock()
			if ch != nil {
				select {
				case ch <- f:
				default:
				}
			}
			continue
		}
		switch f.T {
		case "welcome":
			if !hasFeature(f.Features, "bind") || !hasFeature(f.Features, "deliver") {
				return fmt.Errorf("hub lacks outpost mail features; upgrade hub before worker")
			}
			welcomed()
			w.mu.Lock()
			w.ready = true
			w.mu.Unlock()
			w.Config.WorkerID = f.WorkerID
			_ = SaveJSON(filepath.Join(w.Root, "config.json"), w.Config)
			if w.OnConnect != nil {
				w.OnConnect()
			}
			select {
			case w.commands <- f:
			default:
				return fmt.Errorf("worker command queue full")
			}
		case "ping":
			if err = w.Send(workerwire.Frame{T: "pong", TS: f.TS}); err != nil {
				return err
			}
		default:
			if w.Command != nil {
				if f.T == "launch" {
					select {
					case w.launches <- struct{}{}:
						// Preserve main's deadline and concurrent stop/input handling,
						// while bounding how many launches can run off the reader.
						go func(f workerwire.Frame) {
							defer func() { <-w.launches }()
							lctx, cancel := context.WithTimeout(ctx, WorkerLaunchTimeout)
							defer cancel()
							if err := w.Command(lctx, f); err != nil {
								_ = w.Send(workerwire.Frame{T: "launch_failed", CmdID: f.CmdID, Session: f.Session, Error: err.Error()})
							}
						}(f)
					default:
						_ = w.Send(workerwire.Frame{T: "launch_failed", CmdID: f.CmdID, Session: f.Session, Error: "worker launch capacity full"})
					}
					continue
				}
				select {
				case w.commands <- f:
				default:
					_ = w.Send(workerwire.Frame{T: "launch_failed", CmdID: f.CmdID, Session: f.Session, Error: "worker command queue full"})
				}
			} else {
				_ = w.Send(workerwire.Frame{T: "launch_failed", CmdID: f.CmdID, Session: f.Session, Error: "execution not installed"})
			}
		}
	}
}

func hasFeature(features []string, want string) bool {
	for _, v := range features {
		if v == want {
			return true
		}
	}
	return false
}

func (w *Worker) Request(ctx context.Context, f workerwire.Frame) (workerwire.Frame, error) {
	ch := make(chan workerwire.Frame, 1)
	w.pendingMu.Lock()
	if w.pending == nil {
		w.pending = map[string]chan workerwire.Frame{}
	}
	w.pending[f.CmdID] = ch
	w.pendingMu.Unlock()
	defer func() { w.pendingMu.Lock(); delete(w.pending, f.CmdID); w.pendingMu.Unlock() }()
	if err := w.Send(f); err != nil {
		return workerwire.Frame{}, err
	}
	select {
	case r := <-ch:
		if r.Error != "" {
			return r, fmt.Errorf("%s", r.Error)
		}
		return r, nil
	case <-ctx.Done():
		return workerwire.Frame{}, ctx.Err()
	}
}
