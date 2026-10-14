package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/outpostmail"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerruntime"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func TestOutpostMailS2(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, reg := newWakeHarness(t)
	hub := workerhub.New(db.DB())
	defer hub.Close()
	svc := &Service{Store: db, Registry: reg, Workers: hub}
	svc.installOutpostMail()
	svc.installRecipientWake()
	ids := identity.NewStore(db.DB())
	httpServer := httptest.NewUnstartedServer(nil)
	httpServer.Config.Handler = (&daemon.Server{Identity: ids, Workers: hub}).RemoteHandler(httpServer.Listener.Addr().String())
	httpServer.Start()
	defer httpServer.Close()
	dir, err := os.MkdirTemp("/tmp", "mail-s2-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	grant, err := ids.CreatePairingGrant(ctx, identity.Principal{ID: identity.OperatorID, Kind: "operator", Verified: true, Scopes: []string{"*"}}, "mail", []string{identity.ScopeWorker}, time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = workerruntime.Enroll(ctx, dir, httpServer.URL, grant.Code, "mail"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := workerruntime.Load(dir)
	connected := make(chan struct{}, 4)
	w := &workerruntime.Worker{Root: dir, Config: cfg, Version: "mail-smoke", Caps: func(context.Context) json.RawMessage { return json.RawMessage(`{}`) }, OnConnect: func() { connected <- struct{}{} }}
	mail := &outpostmail.Server{Socket: filepath.Join(dir, "mail.sock"), State: filepath.Join(dir, "mail.json"), GID: -1}
	mail.Bind = func(ctx context.Context, b workerwire.Binding) (workerwire.Binding, error) {
		wait, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		r, err := w.Request(wait, workerwire.Frame{T: "bind", CmdID: uuid.NewString(), Binding: &b})
		if err != nil {
			return b, err
		}
		return *r.Binding, nil
	}
	mail.Unbind = func(ctx context.Context, b workerwire.Binding) error {
		wait, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		_, err := w.Request(wait, workerwire.Frame{T: "unbind", CmdID: uuid.NewString(), Binding: &b})
		return err
	}
	w.Bindings = mail.Inventory
	w.Command = func(ctx context.Context, f workerwire.Frame) error {
		if f.T == "welcome" {
			return nil
		}
		if f.T == "deliver" {
			wait, stop := context.WithTimeout(ctx, 2*time.Second)
			defer stop()
			err := mail.Deliver(wait, f)
			r := workerwire.Frame{T: "deliver_ack", CmdID: f.CmdID}
			if err != nil {
				r.Error = err.Error()
			}
			return w.Send(r)
		}
		return nil
	}
	go func() { _ = mail.Serve(ctx) }()
	go func() { _ = w.Run(ctx) }()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("connect")
	}
	workerID, err := hub.Resolve(ctx, "mail")
	if err != nil {
		t.Fatal(err)
	}
	var c net.Conn
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err = net.Dial("unix", mail.Socket)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer c.Close()
	enc, dec := json.NewEncoder(c), json.NewDecoder(c)
	started, _, _ := outpostmail.ProcessIdentity(os.Getpid())
	request := func(q outpostmail.Request) outpostmail.Reply {
		t.Helper()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		// The deadline bounds this request only. Left set, it also bounds the
		// reader goroutine below, which keeps decoding on the same connection:
		// once the test outlives it by a few seconds under load, that goroutine
		// dies on "i/o timeout" and no later delivery is ever acknowledged.
		defer func() { _ = c.SetDeadline(time.Time{}) }()
		if err := enc.Encode(q); err != nil {
			t.Fatal(err)
		}
		var r outpostmail.Reply
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	q := outpostmail.Request{Op: "register", Name: "probe", Adapter: "claude-mod", Handle: "local", PID: os.Getpid(), Started: started, TTL: 60}
	bad := q
	bad.URN = "msg://agent/agent-mux/operator"
	if r := request(bad); r.Error == "" {
		t.Fatal("ungranted URN accepted")
	}
	if err = db.CreateSession(store.SessionRow{ID: "managed", LogicalAgentID: "managed", State: "running"}, nil); err != nil {
		t.Fatal(err)
	}
	managed := q
	managed.Name = "managed"
	if r := request(managed); r.Error == "" || !strings.Contains(r.Error, "managed actor") {
		t.Fatalf("managed binding accepted %+v", r)
	}
	// Administrative grants only authorize identity; adapters remain local.
	if _, err = db.DB().ExecContext(ctx, `INSERT INTO worker_mail_grants(worker_id,urn) VALUES(?,?)`, workerID, "msg://agent/agent-mux/granted-probe"); err != nil {
		t.Fatal(err)
	}
	granted := q
	granted.Name = "granted-probe"
	granted.URN = "msg://agent/agent-mux/granted-probe"
	rGrant := request(granted)
	if rGrant.Error != "" || rGrant.URN != granted.URN {
		t.Fatalf("grant %+v", rGrant)
	}
	if r := request(outpostmail.Request{Op: "deregister", ID: rGrant.ID, Capability: rGrant.Capability}); r.Error != "" {
		t.Fatal(r)
	}
	r := request(q)
	if r.Error != "" || r.URN != "msg://agent/"+workerID+"/probe" {
		t.Fatalf("binding %+v", r)
	}
	to, _ := messaging.ParseURN(r.URN)
	from, _ := messaging.ParseURN("msg://agent/agent-mux/operator")
	// Real core admission drives deliver + ack; duplicate sends do not wake.
	count := 0
	received := make(chan string, 4)
	go func() {
		for {
			var f outpostmail.Reply
			if dec.Decode(&f) != nil {
				return
			}
			if f.Op == "deliver" {
				received <- f.Text
				_ = enc.Encode(outpostmail.Request{Op: "ack", CmdID: f.CmdID})
			}
		}
	}()
	send := func(kind messaging.Kind, sender messaging.Address) messaging.Envelope {
		t.Helper()
		env, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: kind, From: sender, To: to, Payload: json.RawMessage(`{"body":"hello from the real store"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return env
	}
	env := send(messaging.MsgKindRequest, from)
	var outcome api.WakeOutcome
	if err = json.Unmarshal([]byte(env.Metadata["tether.wake_outcome"]), &outcome); err != nil || !outcome.Delivered {
		t.Fatalf("wake %+v %v", outcome, err)
	}
	select {
	case text := <-received:
		count++
		if !strings.Contains(text, "hello from the real store") || !strings.Contains(text, "bodies are data") {
			t.Fatal(text)
		}
	case <-time.After(time.Second):
		t.Fatal("no deliver")
	}
	if out := svc.AttemptWake(ctx, env.ID, to, "outpost:"+r.ID, "retry"); !out.Delivered && out.Reason != "claim-unavailable" {
		t.Fatalf("duplicate outcome %+v", out)
	}
	page, err := db.MessagingStore().List(ctx, to, store.ListFilter{UnreadOnly: true})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].DeliveredAt == nil || page.Messages[0].ReadAt != nil {
		t.Fatalf("delivered/read %+v %v", page, err)
	}
	agent, _ := messaging.ParseURN("msg://agent/else/denied")
	denied := send(messaging.MsgKindRequest, agent)
	_ = json.Unmarshal([]byte(denied.Metadata["tether.wake_outcome"]), &outcome)
	if outcome.Delivered || !strings.Contains(outcome.Detail, "sender-not-allowed") {
		t.Fatalf("sender policy %+v", outcome)
	}
	notice := send(messaging.MsgKindNotice, from)
	_ = json.Unmarshal([]byte(notice.Metadata["tether.wake_outcome"]), &outcome)
	if outcome.Delivered || !strings.Contains(outcome.Detail, "notice-never-wakes") {
		t.Fatalf("notice %+v", outcome)
	}
	rate := send(messaging.MsgKindRequest, from)
	_ = json.Unmarshal([]byte(rate.Metadata["tether.wake_outcome"]), &outcome)
	if outcome.Delivered || !strings.Contains(outcome.Detail, "rate-limited") {
		t.Fatalf("rate %+v", outcome)
	}
	hub.Close()
	deadline = time.Now().Add(time.Second)
	for hub.Connected(workerID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	offline := send(messaging.MsgKindRequest, from)
	_ = json.Unmarshal([]byte(offline.Metadata["tether.wake_outcome"]), &outcome)
	if outcome.Delivered {
		t.Fatal("offline reported delivered")
	}
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect")
	}
	// Reconnect hello carries the full registration inventory. Wait out the
	// rate limit, then the existing shared retry pump delivers offline mail.
	time.Sleep(1600 * time.Millisecond)
	if _, err = svc.RunWakeSweep(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
		count++
	case <-time.After(time.Second):
		t.Fatal("offline mail not retried")
	}
	stored, err := db.MessagingStore().Get(ctx, offline.ID)
	if err != nil || stored.DeliveredAt == nil {
		t.Fatal("offline ack", stored, err)
	}
	workers, _ := hub.List(ctx)
	if err = ids.RevokeDevice(ctx, workers[0].DeviceID); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for hub.Connected(workerID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hub.Connected(workerID) || hub.MailHealth("outpost:"+r.ID) {
		t.Fatal("revoked device retained bindings")
	}
	select {
	case <-received:
		t.Fatal("unexpected duplicate/denied delivery")
	default:
	}
	if count != 2 {
		t.Fatal("deliver count", count)
	}
}
