package outpostmail

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/workerwire"
)

func connectSmoke(t *testing.T, path string) *net.UnixConn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.DialUnix("unix", nil, &net.UnixAddr{Net: "unix", Name: path})
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestOutpostMailS1(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mail-s1-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s := &Server{Socket: filepath.Join(dir, "mail.sock"), State: filepath.Join(dir, "state.json"), GID: -1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	c := connectSmoke(t, s.Socket)
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	enc, dec := json.NewEncoder(c), json.NewDecoder(c)
	request := func(q Request) Reply {
		t.Helper()
		if err := enc.Encode(q); err != nil {
			t.Fatal(err)
		}
		var r Reply
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	started, _, err := ProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	q := Request{Op: "register", Name: "probe", Adapter: "claude-mod", Handle: "native", PID: os.Getpid(), Started: started, TTL: 60}
	bad := q
	bad.Started = "recycled-pid"
	if r := request(bad); r.Error == "" {
		t.Fatal("wrong process identity accepted")
	}
	r := request(q)
	if r.Error != "" || r.Capability == "" {
		t.Fatalf("register %+v", r)
	}
	if got := request(Request{Op: "heartbeat", ID: r.ID, Capability: "not-the-capability"}); got.Error == "" {
		t.Fatal("foreign renewal accepted")
	}
	if got := request(Request{Op: "deregister", ID: r.ID, Capability: "not-the-capability"}); got.Error == "" {
		t.Fatal("foreign deregister accepted")
	}
	if got := request(q); got.Error == "" {
		t.Fatal("duplicate live registration accepted")
	}
	if info, err := os.Stat(s.Socket); err != nil || info.Mode().Perm() != 0660 {
		t.Fatal("socket permissions", info, err)
	}
	if info, err := os.Stat(s.State); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("state permissions", info, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c.Close()
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	s = &Server{Socket: s.Socket, State: s.State, GID: -1}
	go func() { done <- s.Serve(ctx) }()
	c = connectSmoke(t, s.Socket)
	defer c.Close()
	enc, dec = json.NewEncoder(c), json.NewDecoder(c)
	if got := request(Request{Op: "heartbeat", ID: r.ID, Capability: r.Capability}); got.ID != r.ID {
		t.Fatalf("restart lost registration %+v", got)
	}
	s.mu.Lock()
	s.regs[r.ID].Expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if got := request(Request{Op: "heartbeat", ID: r.ID, Capability: r.Capability}); got.Error == "" {
		t.Fatal("expired lease accepted")
	}
	cancel()
	<-done
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	s = &Server{Socket: s.Socket, State: s.State, GID: -1, AllowedUIDs: []int{os.Getuid() + 10000}}
	go func() { done <- s.Serve(ctx) }()
	c = connectSmoke(t, s.Socket)
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	_ = json.NewEncoder(c).Encode(q)
	var deny Reply
	if err = json.NewDecoder(c).Decode(&deny); err == nil {
		t.Fatal("nonallowlisted peer connected")
	}
	cancel()
	<-done
}

func TestOutpostMailDedupe(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mail-ack-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s := &Server{Socket: filepath.Join(dir, "mail.sock"), State: filepath.Join(dir, "state.json"), GID: -1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	c := connectSmoke(t, s.Socket)
	defer c.Close()
	enc, dec := json.NewEncoder(c), json.NewDecoder(c)
	started, _, _ := ProcessIdentity(os.Getpid())
	_ = enc.Encode(Request{Op: "register", Name: "probe", Adapter: "claude-mod", Handle: "native", PID: os.Getpid(), Started: started})
	var r Reply
	if err = dec.Decode(&r); err != nil || r.Error != "" {
		t.Fatal(err, r)
	}
	f := workerwire.Frame{T: "deliver", CmdID: "cmd", MessageID: "msg", Binding: &workerwire.Binding{ID: r.ID}, Text: "test"}
	done := make(chan error, 1)
	go func() { done <- s.Deliver(ctx, f) }()
	var delivery Reply
	if err = dec.Decode(&delivery); err != nil {
		t.Fatal(err)
	}
	if err = Acknowledge(ctx, s.Socket, Request{ID: r.ID, Capability: "wrong", CmdID: delivery.CmdID}); err == nil {
		t.Fatal("cross-connection ack without capability")
	}
	if err = Acknowledge(ctx, s.Socket, Request{ID: r.ID, Capability: r.Capability, CmdID: delivery.CmdID}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = s.Deliver(ctx, f); err != nil {
		t.Fatal("dedupe failed", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if dec.Decode(&delivery) == nil {
		t.Fatal("duplicate delivered")
	}
}
