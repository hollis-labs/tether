// Package workerhub owns the hub's live outpost channels, never peer authorities.
package workerhub

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/workerwire"
)

type Record struct {
	ID        string          `json:"id"`
	Label     string          `json:"label"`
	DeviceID  string          `json:"device_id"`
	Platform  string          `json:"platform"`
	Caps      json.RawMessage `json:"caps"`
	LastSeen  string          `json:"last_seen"`
	Connected bool            `json:"connected"`
	Sessions  int             `json:"sessions"`
}
type connection struct {
	ws        *websocket.Conn
	write     sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan workerwire.Frame
	done      chan struct{}
}

func (c *connection) send(f workerwire.Frame) error {
	c.write.Lock()
	defer c.write.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteJSON(f)
}

type Hub struct {
	db          *sql.DB
	mu          sync.RWMutex
	connections map[string]*connection
	// Event is installed by the app for worker-bound output/state ingestion.
	Event func(string, workerwire.Frame) error
}

func New(db *sql.DB) *Hub { return &Hub{db: db, connections: make(map[string]*connection)} }
func (h *Hub) Close() {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.connections {
		_ = c.ws.Close()
	}
}
func (h *Hub) Connected(id string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.connections[id] != nil
}
func (h *Hub) Resolve(ctx context.Context, name string) (string, error) {
	var id string
	err := h.db.QueryRowContext(ctx, `SELECT id FROM workers WHERE id=? OR label=? ORDER BY id LIMIT 1`, name, name).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("worker %q not enrolled", name)
	}
	return id, nil
}
func (h *Hub) List(ctx context.Context) ([]Record, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT w.id,w.label,w.device_id,w.platform,w.caps_json,w.last_seen,(SELECT COUNT(*) FROM sessions s WHERE s.worker_id=w.id AND s.state IN ('launching','running','idle')) FROM workers w ORDER BY label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Record{}
	for rows.Next() {
		var r Record
		var caps string
		if err := rows.Scan(&r.ID, &r.Label, &r.DeviceID, &r.Platform, &caps, &r.LastSeen, &r.Sessions); err != nil {
			return nil, err
		}
		r.Caps = json.RawMessage(caps)
		r.Connected = h.Connected(r.ID)
		result = append(result, r)
	}
	return result, rows.Err()
}
func (h *Hub) ListHTTP(w http.ResponseWriter, r *http.Request) {
	records, err := h.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"workers": records})
}
func (h *Hub) Connect(w http.ResponseWriter, r *http.Request) {
	p, ok := identity.FromContext(r.Context())
	if !ok || !p.Verified || !identity.HasDeviceScope(p, identity.ScopeWorker) {
		http.Error(w, "worker device required", 403)
		return
	}
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(8 << 20)
	_ = ws.SetReadDeadline(time.Now().Add(45 * time.Second))
	var hello workerwire.Frame
	if ws.ReadJSON(&hello) != nil || hello.T != "hello" || hello.Proto != workerwire.Protocol || hello.Worker == nil || hello.Worker.Label == "" {
		return
	}
	sum := sha256.Sum256([]byte(p.ID))
	id := "worker_" + hex.EncodeToString(sum[:12])
	now := time.Now().UTC().Format(time.RFC3339Nano)
	caps := string(hello.Caps)
	if caps == "" {
		caps = "{}"
	}
	_, err = h.db.ExecContext(r.Context(), `INSERT INTO workers(id,label,device_id,platform,caps_json,last_seen,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(device_id) DO UPDATE SET label=excluded.label,platform=excluded.platform,caps_json=excluded.caps_json,last_seen=excluded.last_seen`, id, hello.Worker.Label, p.ID, hello.Worker.Platform+"/"+hello.Worker.Arch, caps, now, now)
	if err != nil {
		return
	}
	c := &connection{ws: ws, pending: make(map[string]chan workerwire.Frame), done: make(chan struct{})}
	h.mu.Lock()
	old := h.connections[id]
	h.connections[id] = c
	h.mu.Unlock()
	if old != nil {
		_ = old.ws.Close()
	}
	defer func() {
		close(c.done)
		h.mu.Lock()
		if h.connections[id] == c {
			delete(h.connections, id)
		}
		h.mu.Unlock()
	}()
	welcome := workerwire.Frame{T: "welcome", WorkerID: id}
	for _, held := range hello.Sessions {
		var state string
		var seq int64
		err := h.db.QueryRowContext(r.Context(), `SELECT s.state,COALESCE(q.last_seq,0) FROM sessions s LEFT JOIN session_worker_seq q ON q.session_id=s.id WHERE s.id=? AND s.worker_id=?`, held.ID, id).Scan(&state, &seq)
		if err != nil || state == "exited" || state == "failed" || state == "stopped" {
			welcome.Kill = append(welcome.Kill, held.ID)
		} else {
			welcome.Resume = append(welcome.Resume, workerwire.Cursor{Session: held.ID, FromSeq: seq + 1})
		}
	}
	if c.send(welcome) != nil {
		return
	}
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-ticker.C:
				if c.send(workerwire.Frame{T: "ping", TS: time.Now().Unix()}) != nil {
					_ = ws.Close()
					return
				}
			}
		}
	}()
	for {
		var f workerwire.Frame
		if ws.ReadJSON(&f) != nil {
			return
		}
		_ = ws.SetReadDeadline(time.Now().Add(45 * time.Second))
		_, _ = h.db.Exec(`UPDATE workers SET last_seen=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), id)
		if f.CmdID != "" {
			c.pendingMu.Lock()
			ch := c.pending[f.CmdID]
			c.pendingMu.Unlock()
			if ch != nil {
				select {
				case ch <- f:
				default:
				}
			}
		}
		if h.Event != nil && f.Session != "" {
			if err := h.Event(id, f); err != nil {
				_ = c.send(workerwire.Frame{T: "replay", Session: f.Session, FromSeq: h.NextSeq(f.Session)})
			}
		}
	}
}
func (h *Hub) NextSeq(session string) int64 {
	var seq int64
	_ = h.db.QueryRow(`SELECT last_seq FROM session_worker_seq WHERE session_id=?`, session).Scan(&seq)
	return seq + 1
}
func (h *Hub) Send(ctx context.Context, id string, f workerwire.Frame) (workerwire.Frame, error) {
	h.mu.RLock()
	c := h.connections[id]
	h.mu.RUnlock()
	if c == nil {
		return workerwire.Frame{}, fmt.Errorf("worker offline")
	}
	if f.CmdID == "" {
		return workerwire.Frame{}, c.send(f)
	}
	ch := make(chan workerwire.Frame, 1)
	c.pendingMu.Lock()
	c.pending[f.CmdID] = ch
	c.pendingMu.Unlock()
	defer func() { c.pendingMu.Lock(); delete(c.pending, f.CmdID); c.pendingMu.Unlock() }()
	if err := c.send(f); err != nil {
		return workerwire.Frame{}, err
	}
	select {
	case response := <-ch:
		if response.Error != "" {
			return response, fmt.Errorf("%s", response.Error)
		}
		return response, nil
	case <-ctx.Done():
		return workerwire.Frame{}, ctx.Err()
	case <-c.done:
		return workerwire.Frame{}, fmt.Errorf("worker disconnected")
	}
}
