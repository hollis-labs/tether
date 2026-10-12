package workerhub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/workerwire"
)

type mailBinding struct {
	workerwire.Binding
	WorkerID     string
	Expires      time.Time
	LastDelivery time.Time
}

func feature(v []string, want string) bool {
	for _, s := range v {
		if s == want {
			return true
		}
	}
	return false
}

func (h *Hub) bind(ctx context.Context, id string, c *connection, b workerwire.Binding) (workerwire.Binding, error) {
	if !feature(c.features, "bind") || !feature(c.features, "deliver") {
		return b, fmt.Errorf("worker lacks bind/deliver features")
	}
	if b.ID == "" || b.Handle == "" || strings.ContainsAny(b.Handle, "/\\?# ") || strings.HasPrefix(b.Handle, "-") {
		return b, fmt.Errorf("invalid registration id/name")
	}
	minted := "msg://agent/" + id + "/" + b.Handle
	if b.URN == "" {
		b.URN = minted
	}
	if b.URN != minted {
		var exists int
		if err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_mail_grants WHERE worker_id=? AND urn=?`, id, b.URN).Scan(&exists); err != nil {
			return b, err
		}
		if exists != 1 {
			return b, fmt.Errorf("URN requires explicit worker grant")
		}
	}
	b.TTL = max(15, min(b.TTL, 120))
	h.mailMu.Lock()
	defer h.mailMu.Unlock()
	old := h.mail[b.URN]
	if old != nil && time.Now().Before(old.Expires) && (old.ID != b.ID || old.WorkerID != id) {
		return b, fmt.Errorf("URN already has a live Outpost registration")
	}
	if old != nil && old.ID == b.ID && old.WorkerID == id {
		b.Generation = old.Generation
	}
	if h.BindMail == nil {
		return b, fmt.Errorf("hub mail routing unavailable")
	}
	var err error
	b, err = h.BindMail(ctx, id, b)
	if err != nil {
		return b, err
	}
	next := &mailBinding{Binding: b, WorkerID: id, Expires: time.Now().Add(time.Duration(b.TTL) * time.Second)}
	if old != nil {
		next.LastDelivery = old.LastDelivery
	}
	h.mail[b.URN] = next
	return b, nil
}
func (h *Hub) unbind(ctx context.Context, id string, b workerwire.Binding) error {
	h.mailMu.Lock()
	defer h.mailMu.Unlock()
	current := h.mail[b.URN]
	if current == nil || current.WorkerID != id || current.ID != b.ID {
		return nil
	}
	if h.UnbindMail != nil {
		if err := h.UnbindMail(ctx, id, current.Binding); err != nil {
			return err
		}
	}
	delete(h.mail, b.URN)
	return nil
}
func (h *Hub) disconnectMail(id string) {
	h.mailMu.Lock()
	defer h.mailMu.Unlock()
	for urn, b := range h.mail {
		if b.WorkerID == id {
			if h.UnbindMail != nil {
				_ = h.UnbindMail(context.Background(), id, b.Binding)
			}
			delete(h.mail, urn)
		}
	}
}
func (h *Hub) MailRoute(urn string) (string, bool) {
	h.mailMu.Lock()
	defer h.mailMu.Unlock()
	b := h.mail[urn]
	if b == nil || time.Now().After(b.Expires) {
		return "", false
	}
	return "outpost:" + b.ID, true
}
func (h *Hub) mailSession(session string) (mailBinding, bool) {
	h.mailMu.Lock()
	defer h.mailMu.Unlock()
	for _, b := range h.mail {
		if "outpost:"+b.ID == session && time.Now().Before(b.Expires) {
			return *b, true
		}
	}
	return mailBinding{}, false
}
func (h *Hub) MailHealth(session string) bool {
	b, ok := h.mailSession(session)
	return ok && h.Connected(b.WorkerID)
}
func (h *Hub) Deliver(ctx context.Context, session, msgID, text string) error {
	b, ok := h.mailSession(session)
	if !ok {
		return fmt.Errorf("outpost binding offline")
	}
	_, err := h.Send(ctx, b.WorkerID, workerwire.Frame{T: "deliver", CmdID: msgID, MessageID: msgID, URN: b.URN, Binding: &b.Binding, Text: text})
	return err
}

// Allow checks only wake authority, never mailbox visibility. Claiming sender
// identity is still the existing self-asserted convention; verified senders
// are post-MVP. Operator = the exact canonical operator URN, not a substring.
func (h *Hub) Allow(session, from, kind string) error {
	h.mailMu.Lock()
	defer h.mailMu.Unlock()
	for _, b := range h.mail {
		if "outpost:"+b.ID != session {
			continue
		}
		if kind == "notice" {
			return fmt.Errorf("notice-never-wakes")
		}
		allowed := from == "msg://agent/agent-mux/operator" || from == "msg://user/local/operator"
		for _, sender := range b.AllowedSenders {
			allowed = allowed || sender == from
		}
		if !allowed {
			return fmt.Errorf("sender-not-allowed")
		}
		if time.Since(b.LastDelivery) < 1500*time.Millisecond {
			return fmt.Errorf("outpost-rate-limited")
		}
		b.LastDelivery = time.Now()
		return nil
	}
	return fmt.Errorf("outpost binding offline")
}

func (h *Hub) GrantsHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Worker string `json:"worker"`
		URN    string `json:"urn"`
		Remove bool   `json:"remove"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// Grants are authority changes: labels are not unique. Require the exact
	// worker id, never Resolve's label convenience used by ordinary launches.
	var id string
	err := h.db.QueryRowContext(r.Context(), `SELECT id FROM workers WHERE id=?`, in.Worker).Scan(&id)
	if err != nil || !strings.HasPrefix(in.URN, "msg://agent/") || len(strings.Split(in.URN, "/")) != 5 {
		http.Error(w, "valid worker and agent URN required", 400)
		return
	}
	if in.Remove {
		_, err = h.db.ExecContext(r.Context(), `DELETE FROM worker_mail_grants WHERE worker_id=? AND urn=?`, id, in.URN)
	} else {
		_, err = h.db.ExecContext(r.Context(), `INSERT OR IGNORE INTO worker_mail_grants(worker_id,urn) VALUES(?,?)`, id, in.URN)
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if in.Remove {
		h.mailMu.Lock()
		if b := h.mail[in.URN]; b != nil && b.WorkerID == id {
			if h.UnbindMail != nil {
				_ = h.UnbindMail(r.Context(), id, b.Binding)
			}
			delete(h.mail, in.URN)
		}
		h.mailMu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"worker_id": id, "urn": in.URN, "removed": in.Remove})
}
