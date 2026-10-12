// Package outpostmail is the local, credential-free registration/delivery
// surface. It is not a Tether authority. The hub never sees local adapter data.
package outpostmail

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/workerwire"
)

type Request struct {
	Op             string   `json:"op"`
	ID             string   `json:"id,omitempty"`
	Capability     string   `json:"capability,omitempty"`
	URN            string   `json:"urn,omitempty"`
	Name           string   `json:"name,omitempty"`
	Adapter        string   `json:"adapter,omitempty"`
	Handle         string   `json:"handle,omitempty"`
	PID            int      `json:"pid,omitempty"`
	Started        string   `json:"pid_start_time,omitempty"`
	TTL            int      `json:"ttl,omitempty"`
	AllowedSenders []string `json:"allowed_senders,omitempty"`
	CmdID          string   `json:"cmd_id,omitempty"`
	Error          string   `json:"error,omitempty"`
	Text           string   `json:"text,omitempty"`
	EscapeFirst    bool     `json:"escape_first,omitempty"`
}

var ErrHubOffline = errors.New("hub offline; registration retained locally")

func (s *Server) Welcome(bindings []workerwire.Binding) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range bindings {
		if r := s.regs[b.ID]; r != nil {
			r.URN = b.URN
		}
	}
	_ = s.persist()
}

type Reply struct {
	Op         string `json:"op"`
	ID         string `json:"id,omitempty"`
	Capability string `json:"capability,omitempty"`
	URN        string `json:"urn,omitempty"`
	CmdID      string `json:"cmd_id,omitempty"`
	Text       string `json:"text,omitempty"`
	Error      string `json:"error,omitempty"`
	TTL        int    `json:"ttl,omitempty"`
}
type registration struct {
	Request
	UID       int       `json:"uid"`
	Expires   time.Time `json:"expires"`
	Delivered []string  `json:"delivered,omitempty"`
	transport *transport
}
type transport struct {
	c         *net.UnixConn
	mu        sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan Request
}

func (t *transport) send(r Reply) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return json.NewEncoder(t.c).Encode(r)
}

type Server struct {
	delivery      sync.Mutex
	Socket, State string
	GID           int
	AllowedUIDs   []int
	Bind          func(context.Context, workerwire.Binding) (workerwire.Binding, error)
	Unbind        func(context.Context, workerwire.Binding) error
	mu            sync.Mutex
	regs          map[string]*registration
}

func (s *Server) load() error {
	s.regs = map[string]*registration{}
	b, err := os.ReadFile(s.State)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, &s.regs); err != nil {
		return err
	}
	for id, r := range s.regs {
		if !s.live(r) {
			delete(s.regs, id)
		}
	}
	return nil
}
func (s *Server) persist() error {
	if err := os.MkdirAll(filepath.Dir(s.State), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.State), ".mail-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(s.regs)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), s.State)
}
func (s *Server) live(r *registration) bool {
	if time.Now().After(r.Expires) {
		return false
	}
	started, uid, err := ProcessIdentity(r.PID)
	return err == nil && uid == r.UID && started == r.Started
}
func (s *Server) binding(r *registration) workerwire.Binding {
	return workerwire.Binding{ID: r.ID, URN: r.URN, Handle: r.Name, TTL: r.TTL, AllowedSenders: r.AllowedSenders}
}
func (s *Server) Inventory() []workerwire.Binding {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []workerwire.Binding
	for _, r := range s.regs {
		if r.transport != nil && s.live(r) {
			out = append(out, s.binding(r))
		}
	}
	return out
}
func (s *Server) Serve(ctx context.Context) error {
	s.mu.Lock()
	if err := s.load(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	if len(s.AllowedUIDs) == 0 {
		s.AllowedUIDs = []int{os.Getuid()}
	}
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0750); err != nil {
		return err
	}
	// Refuse a second outpost rather than unlink its live socket.
	if c, err := net.DialTimeout("unix", s.Socket, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("mail socket already active")
	}
	if info, err := os.Lstat(s.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("mail socket path is not a socket")
		}
		if err = os.Remove(s.Socket); err != nil {
			return err
		}
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.Socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer l.Close()
	if s.GID >= 0 {
		if err = os.Chown(s.Socket, -1, s.GID); err != nil {
			return err
		}
	}
	if err = os.Chmod(s.Socket, 0660); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	go s.sweep(ctx)
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.serveConn(ctx, c)
	}
}
func (s *Server) sweep(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.mu.Lock()
			var expired []workerwire.Binding
			for id, r := range s.regs {
				if !s.live(r) {
					expired = append(expired, s.binding(r))
					if r.transport != nil {
						r.transport.c.Close()
					}
					delete(s.regs, id)
				}
			}
			if len(expired) > 0 {
				_ = s.persist()
			}
			s.mu.Unlock()
			for _, b := range expired {
				if s.Unbind != nil {
					_ = s.Unbind(ctx, b)
				}
			}
		}
	}
}
func (s *Server) serveConn(ctx context.Context, c *net.UnixConn) {
	defer c.Close()
	uid, err := peerUID(c)
	if err != nil {
		return
	}
	allowed := false
	for _, u := range s.AllowedUIDs {
		allowed = allowed || u == uid
	}
	if !allowed {
		return
	}
	t := &transport{c: c, pending: map[string]chan Request{}}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	defer func() {
		s.mu.Lock()
		for _, r := range s.regs {
			if r.transport == t {
				r.transport = nil
			}
		}
		s.mu.Unlock()
	}()
	scan := bufio.NewScanner(c)
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		var req Request
		if err = json.Unmarshal(scan.Bytes(), &req); err != nil {
			_ = t.send(Reply{Op: "error", Error: err.Error()})
			continue
		}
		if req.Op == "ack" {
			t.pendingMu.Lock()
			ch := t.pending[req.CmdID]
			t.pendingMu.Unlock()
			if ch == nil {
				s.mu.Lock()
				reg := s.regs[req.ID]
				if reg != nil && reg.UID == uid && reg.Capability == req.Capability && s.live(reg) && reg.transport != nil {
					reg.transport.pendingMu.Lock()
					ch = reg.transport.pending[req.CmdID]
					reg.transport.pendingMu.Unlock()
				}
				s.mu.Unlock()
				if ch == nil {
					_ = t.send(Reply{Op: "error", Error: "invalid acknowledgement capability/command"})
					continue
				}
				select {
				case ch <- req:
				default:
				}
				_ = t.send(Reply{Op: "acknowledged", CmdID: req.CmdID})
				continue
			}
			if ch != nil {
				select {
				case ch <- req:
				default:
				}
			}
			continue
		}
		reply := s.request(ctx, uid, t, req)
		if t.send(reply) != nil {
			return
		}
	}
}
func (s *Server) request(ctx context.Context, uid int, t *transport, q Request) Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	fail := func(err error) Reply { return Reply{Op: "error", Error: err.Error()} }
	var r *registration
	if q.Op == "register" {
		for _, existing := range s.regs {
			if s.live(existing) && ((q.URN != "" && existing.URN == q.URN) || existing.Name == q.Name) {
				return fail(fmt.Errorf("agent name/URN already registered; renew it with its capability"))
			}
		}
		if q.Adapter != "tmux-paste" && q.Adapter != "codex-queue" && q.Adapter != "claude-mod" {
			return fail(fmt.Errorf("unsupported adapter"))
		}
		if q.Handle == "" || q.Name == "" {
			return fail(fmt.Errorf("name and adapter handle required"))
		}
		started, owner, err := ProcessIdentity(q.PID)
		if err != nil || owner != uid || started != q.Started {
			return fail(fmt.Errorf("process identity mismatch"))
		}
		secret := make([]byte, 32)
		if _, err = rand.Read(secret); err != nil {
			return fail(err)
		}
		q.ID = uuid.NewString()
		q.Capability = hex.EncodeToString(secret)
		q.TTL = max(15, min(q.TTL, 120))
		if q.TTL == 15 {
			q.TTL = 60
		}
		r = &registration{Request: q, UID: uid, Expires: time.Now().Add(time.Duration(q.TTL) * time.Second), transport: t}
	} else {
		r = s.regs[q.ID]
		if r == nil || r.UID != uid || r.Capability != q.Capability || !s.live(r) {
			return fail(fmt.Errorf("registration capability or lease invalid"))
		}
	}
	switch q.Op {
	case "register", "heartbeat":
		r.transport = t
		r.Expires = time.Now().Add(time.Duration(r.TTL) * time.Second)
		if s.Bind != nil {
			b, err := s.Bind(ctx, s.binding(r))
			if err != nil && !errors.Is(err, ErrHubOffline) {
				if q.Op == "heartbeat" {
					delete(s.regs, r.ID)
					_ = s.persist()
				}
				return fail(err)
			}
			if err == nil {
				r.URN = b.URN
			}
		}
		s.regs[r.ID] = r
		if err := s.persist(); err != nil {
			return fail(err)
		}
		return Reply{Op: "registered", ID: r.ID, Capability: r.Capability, URN: r.URN, TTL: r.TTL}
	case "deregister":
		if s.Unbind != nil {
			if err := s.Unbind(ctx, s.binding(r)); err != nil {
				return fail(err)
			}
		}
		delete(s.regs, r.ID)
		if err := s.persist(); err != nil {
			return fail(err)
		}
		return Reply{Op: "deregistered"}
	default:
		return fail(fmt.Errorf("unknown operation"))
	}
}
func (s *Server) Deliver(ctx context.Context, f workerwire.Frame) error {
	s.delivery.Lock()
	defer s.delivery.Unlock()
	if f.Binding == nil {
		return fmt.Errorf("binding required")
	}
	s.mu.Lock()
	var selected *registration
	for _, r := range s.regs {
		if r.ID == f.Binding.ID && r.URN == f.URN && r.transport != nil && s.live(r) {
			selected = r
			break
		}
	}
	if selected == nil {
		s.mu.Unlock()
		return fmt.Errorf("registration offline")
	}
	for _, id := range selected.Delivered {
		if id == f.MessageID && id != "" {
			s.mu.Unlock()
			return nil
		}
	}
	t := selected.transport
	s.mu.Unlock()
	ch := make(chan Request, 1)
	t.pendingMu.Lock()
	t.pending[f.CmdID] = ch
	t.pendingMu.Unlock()
	defer func() { t.pendingMu.Lock(); delete(t.pending, f.CmdID); t.pendingMu.Unlock() }()
	if err := t.send(Reply{Op: "deliver", CmdID: f.CmdID, Text: f.Text}); err != nil {
		return err
	}
	select {
	case ack := <-ch:
		if ack.Error != "" {
			return fmt.Errorf("%s", ack.Error)
		}
		s.mu.Lock()
		selected.Delivered = append(selected.Delivered, f.MessageID)
		if len(selected.Delivered) > 256 {
			selected.Delivered = selected.Delivered[len(selected.Delivered)-256:]
		}
		err := s.persist()
		s.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
