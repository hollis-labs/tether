package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh/messaging"

	"github.com/hollis-labs/tether/internal/store"
)

type watchFrame struct {
	id, event string
	data      string
}

// watchOpen opens GET /messages/watch and returns a channel of parsed frames.
func watchOpen(t *testing.T, base string, q url.Values) (<-chan watchFrame, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/messages/watch?"+q.Encode(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("watch: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("watch status = %d", resp.StatusCode)
	}
	out := make(chan watchFrame, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		var f watchFrame
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			switch line := sc.Text(); {
			case line == "":
				if f.event != "" {
					out <- f
				}
				f = watchFrame{}
			case strings.HasPrefix(line, "id: "):
				f.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out, func() { cancel() }
}

func nextFrame(t *testing.T, ch <-chan watchFrame) watchFrame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for frame")
	}
	return watchFrame{}
}

func watchQuery(to messaging.Address, after string) url.Values {
	q := url.Values{"to": {to.URN()}, "as": {to.URN()}}
	if after != "" {
		q.Set("after_seq", after)
	}
	return q
}

func TestHandleMessagesWatch_ResumeLiveAndNoSideEffects(t *testing.T) {
	mailboxWatchPoll = 20 * time.Millisecond
	t.Cleanup(func() { mailboxWatchPoll = 500 * time.Millisecond })
	srv, db := newMessageTestServer(t)
	ms := db.MessagingStore()
	alice := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "alice"}
	bob := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "bob"}
	send := func(subject string) messaging.Envelope {
		t.Helper()
		env, err := ms.Send(context.Background(), messaging.Envelope{
			Kind: messaging.MsgKindNotice, From: bob, To: alice,
			Payload: json.RawMessage(`{"subject":"` + subject + `","body":"b-` + subject + `"}`),
		})
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		return env
	}
	first := send("one")
	second := send("two")

	// Replay from 0 yields both, in order, then synchronized.
	ch, stop := watchOpen(t, srv.URL, watchQuery(alice, "0"))
	var seqs []int64
	for i := 0; i < 2; i++ {
		f := nextFrame(t, ch)
		var m struct {
			Seq     int64  `json:"seq"`
			ID      string `json:"id"`
			Subject string `json:"subject"`
			Body    string `json:"body"`
			From    string `json:"from"`
			To      string `json:"to"`
			Kind    string `json:"kind"`
		}
		if f.event != "message" || json.Unmarshal([]byte(f.data), &m) != nil {
			t.Fatalf("frame %d = %+v", i, f)
		}
		want := []messaging.Envelope{first, second}[i]
		if m.ID != want.ID || m.Subject == "" || m.Body == "" || m.From != bob.URN() || m.To != alice.URN() || m.Kind != "notice" || f.id == "" || m.Seq <= 0 {
			t.Fatalf("message %d = %+v (frame %+v)", i, m, f)
		}
		seqs = append(seqs, m.Seq)
	}
	if seqs[1] <= seqs[0] {
		t.Fatalf("seqs not increasing: %v", seqs)
	}
	if f := nextFrame(t, ch); f.event != "synchronized" {
		t.Fatalf("want synchronized, got %+v", f)
	}
	// A live message arrives on the open stream.
	third := send("three")
	f := nextFrame(t, ch)
	if f.event != "message" || !strings.Contains(f.data, third.ID) {
		t.Fatalf("live frame = %+v", f)
	}
	stop()

	// Reconnect after the first seq: no duplicate of the first, second+third replay.
	ch, stop = watchOpen(t, srv.URL, watchQuery(alice, strconv.FormatInt(seqs[0], 10)))
	defer stop()
	for _, want := range []string{second.ID, third.ID} {
		f := nextFrame(t, ch)
		if f.event != "message" || !strings.Contains(f.data, want) || strings.Contains(f.data, first.ID) {
			t.Fatalf("resume frame = %+v, want %s", f, want)
		}
	}

	// Watching never marks anything delivered or read.
	page, err := ms.List(context.Background(), alice, store.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range page.Messages {
		if m.DeliveredAt != nil || m.ReadAt != nil || m.ConsumedAt != nil {
			t.Fatalf("watch changed message state: %+v", m)
		}
	}
	if got, err := ms.Inbox(context.Background(), alice, messaging.Filter{}); err != nil || len(got) != 3 {
		t.Fatalf("inbox after watch = %d, %v; want 3 undelivered", len(got), err)
	}
}

func TestHandleMessagesWatch_FromNowSkipsHistory(t *testing.T) {
	mailboxWatchPoll = 20 * time.Millisecond
	t.Cleanup(func() { mailboxWatchPoll = 500 * time.Millisecond })
	srv, db := newMessageTestServer(t)
	ms := db.MessagingStore()
	alice := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "alice"}
	bob := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "bob"}
	old, _ := ms.Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: bob, To: alice, Payload: json.RawMessage(`{"subject":"old"}`)})
	ch, stop := watchOpen(t, srv.URL, watchQuery(alice, ""))
	defer stop()
	if f := nextFrame(t, ch); f.event != "synchronized" {
		t.Fatalf("first frame = %+v, want synchronized", f)
	}
	fresh, _ := ms.Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: bob, To: alice, Payload: json.RawMessage(`{"subject":"new"}`)})
	f := nextFrame(t, ch)
	if !strings.Contains(f.data, fresh.ID) || strings.Contains(f.data, old.ID) {
		t.Fatalf("frame = %+v", f)
	}
}

func TestHandleMessagesWatch_GapWhenCursorAhead(t *testing.T) {
	srv, _ := newMessageTestServer(t)
	alice := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "alice"}
	ch, stop := watchOpen(t, srv.URL, watchQuery(alice, "999"))
	defer stop()
	f := nextFrame(t, ch)
	if f.event != "gap" || !strings.Contains(f.data, `"reason":"ahead"`) {
		t.Fatalf("frame = %+v", f)
	}
	if _, ok := <-ch; ok {
		t.Fatal("stream should close after gap")
	}
}

func TestHandleMessagesWatch_KeepaliveAndRejections(t *testing.T) {
	mailboxWatchPing = 20 * time.Millisecond
	t.Cleanup(func() { mailboxWatchPing = 15 * time.Second })
	srv, _ := newMessageTestServer(t)
	alice := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "alice"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/messages/watch?"+watchQuery(alice, "").Encode(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() == ": ping" {
			break
		}
	}
	if sc.Text() != ": ping" {
		t.Fatalf("no keepalive: %v", sc.Err())
	}

	for name, tc := range map[string]struct {
		query string
		want  int
	}{
		"no as":       {"to=" + url.QueryEscape(alice.URN()), http.StatusBadRequest},
		"as mismatch": {"to=" + url.QueryEscape(alice.URN()) + "&as=" + url.QueryEscape("msg://agent/test/bob"), http.StatusForbidden},
		"bad after":   {watchQuery(alice, "-1").Encode(), http.StatusBadRequest},
	} {
		r, err := http.Get(srv.URL + "/messages/watch?" + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d", name, r.StatusCode, tc.want)
		}
	}
}

func TestHandleMessagesWatch_RejectsPost(t *testing.T) {
	srv, _ := newMessageTestServer(t)
	alice := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "alice"}
	r, err := http.Post(srv.URL+"/messages/watch?"+watchQuery(alice, "").Encode(), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", r.StatusCode)
	}
}
