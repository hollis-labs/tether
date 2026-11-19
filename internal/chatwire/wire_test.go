package chatwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/ui/ui-go/chatstream"

	"github.com/hollis-labs/tether/internal/api"
	tstore "github.com/hollis-labs/tether/internal/store"
)

// sseEvent is one event off an event stream.
type sseEvent struct {
	ID  uint64
	Ev  chatstream.Event
	Raw string
}

type client struct {
	t    *testing.T
	base string
}

func (c client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// stream opens an attachment's event stream and delivers its events until it ends.
func (c client) stream(conv, att, after string) (<-chan sseEvent, int) {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/chat/v1/conversations/%s/events?attachment=%s&after=%s", c.base, conv, att, after), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, resp.StatusCode
	}
	out := make(chan sseEvent, 256)
	go func() {
		defer close(out)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id:"):
				cur.ID, _ = strconv.ParseUint(strings.TrimSpace(line[3:]), 10, 64)
			case strings.HasPrefix(line, "data:"):
				cur.Raw = strings.TrimSpace(line[5:])
				_ = json.Unmarshal([]byte(cur.Raw), &cur.Ev)
			case line == "" && cur.Raw != "":
				out <- cur
				cur = sseEvent{}
			}
		}
	}()
	return out, http.StatusOK
}

// until reads a stream up to the first event matching stop and returns everything read.
func until(t *testing.T, ch <-chan sseEvent, stop func(chatstream.Event) bool) []sseEvent {
	t.Helper()
	var got []sseEvent
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream ended after %d events, before the expected one", len(got))
			}
			got = append(got, ev)
			if stop(ev.Ev) {
				return got
			}
		case <-deadline:
			t.Fatalf("timed out after %d events", len(got))
		}
	}
}

func generationEvent(ev chatstream.Event) bool {
	return ev.Verb == chatstream.VerbActivity && ev.Kind == "chat.generation"
}

// The flag-on daemon surface end to end: the wire mounted by internal/api, a Claude streaming-stdio seat behind the
// runner, two clients on one conversation, a relaunch that bumps the generation, and a stale attachment refused.
func TestWireIntegration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := tstore.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := newFakeSessions(t)
	sessions.set("s1", RuntimeClaudeStdio)
	(&env{st: st}).session(t, "s1", "seat", "")

	w, err := Open(ctx, Config{StateDir: dir, Store: st, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewHandler(api.Deps{ChatWire: w.Handler()}))
	t.Cleanup(srv.Close)
	// Runs first (cleanups are LIFO): closing the Service ends the open event streams srv.Close would wait for.
	t.Cleanup(func() { srv.CloseClientConnections(); _ = w.Close(context.Background()) })
	c := client{t: t, base: srv.URL}

	// The seat's conversation: the Tether conversation's ID, at its generation.
	status, conv := c.do(http.MethodPost, "/chat/v1/agents/seat/conversation", nil)
	if status != http.StatusOK {
		t.Fatalf("ensure = %d %v", status, conv)
	}
	tether, err := st.ConversationForSeat(ctx, "seat")
	if err != nil {
		t.Fatal(err)
	}
	convID, _ := conv["conversation_id"].(string)
	if convID != tether.ID || conv["generation"] != float64(1) || !strings.HasPrefix(conv["runner_ref"].(string), "s1/") {
		t.Fatalf("conversation = %v; want the Tether conversation %s at generation 1", conv, tether.ID)
	}
	if caps, _ := conv["capabilities"].(map[string]any); caps["deltas"] != true || caps["approvals"] != false {
		t.Fatalf("capabilities = %v", conv["capabilities"])
	}
	if status, _ := c.do(http.MethodPost, "/chat/v1/agents/nobody/conversation", nil); status != http.StatusNotFound {
		t.Fatalf("unknown seat = %d", status)
	}

	// Two clients: a controller and a viewer, both reading from the start.
	attach := func(role string) string {
		t.Helper()
		status, a := c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/attachments", map[string]string{"role": role, "stream": "deltas"})
		if status != http.StatusCreated {
			t.Fatalf("attach %s = %d %v", role, status, a)
		}
		return a["attachment_id"].(string)
	}
	ctl, viewer := attach("controller"), attach("viewer")
	sa, _ := c.stream(convID, ctl, "0")
	sb, _ := c.stream(convID, viewer, "0")

	status, receipt := c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/turns", map[string]any{"attachment": ctl, "text": "hello", "request_id": "r1", "context": json.RawMessage(`{ "view": "private-app-view", "selected": [1, 2] }`)})
	if status != http.StatusAccepted {
		t.Fatalf("submit = %d %v", status, receipt)
	}
	finished := func(ev chatstream.Event) bool { return ev.Verb == chatstream.VerbRunFinish }
	ea, eb := until(t, sa, finished), until(t, sb, finished)
	if len(ea) != len(eb) {
		t.Fatalf("controller saw %d events, viewer %d", len(ea), len(eb))
	}
	var text string
	for i := range ea {
		if ea[i].ID != eb[i].ID || ea[i].Raw != eb[i].Raw {
			t.Fatalf("event %d differs:\n  controller %d %s\n  viewer     %d %s", i, ea[i].ID, ea[i].Raw, eb[i].ID, eb[i].Raw)
		}
		if ev := ea[i].Ev; ev.Verb == chatstream.VerbPartEnd && ev.RunID == receipt["run_id"] {
			var f struct{ Text string }
			_ = json.Unmarshal(ev.Final, &f)
			text += f.Text
		}
	}
	if !strings.Contains(text, "done") {
		t.Fatalf("the run's assistant text = %q, want the fake's answer", text)
	}
	select {
	case input := <-sessions.inputs:
		if input != "<app-context>{\"view\":\"private-app-view\",\"selected\":[1,2]}</app-context>\nhello" {
			t.Fatalf("Claude stdin text = %q", input)
		}
	case <-time.After(time.Second):
		t.Fatal("Claude received no input")
	}
	records := 0
	for rec, err := range w.db.Log.After(ctx, convID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		records++
		if bytes.Contains(rec.Data, []byte("private-app-view")) || bytes.Contains(rec.Data, []byte("app-context")) {
			t.Fatalf("app context leaked into event log: %s", rec.Data)
		}
	}
	if records == 0 {
		t.Fatal("empty conversation event log")
	}
	t.Logf("both clients received the same %d events, ids %d..%d", len(ea), ea[0].ID, ea[len(ea)-1].ID)

	// A relaunch writes a resume session for the seat: the conversation moves in that transaction, the wire hears it
	// after the commit, and the generation rises on the stream.
	sessions.set("s1r", RuntimeClaudeStdio)
	(&env{st: st}).session(t, "s1r", "seat", "s1")
	ga, gb := until(t, sa, generationEvent), until(t, sb, generationEvent)
	for _, g := range [][]sseEvent{ga, gb} {
		var v struct{ Generation uint64 }
		_ = json.Unmarshal(g[len(g)-1].Ev.Value, &v)
		if v.Generation != 2 {
			t.Fatalf("chat.generation value = %s", g[len(g)-1].Ev.Value)
		}
	}
	for name, ch := range map[string]<-chan sseEvent{"controller": sa, "viewer": sb} {
		select {
		case ev, ok := <-ch:
			if ok {
				t.Fatalf("%s stream continued after chat.generation: %s", name, ev.Raw)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s stream still open after chat.generation", name)
		}
	}
	status, conv = c.do(http.MethodGet, "/chat/v1/conversations/"+convID, nil)
	if status != http.StatusOK || conv["generation"] != float64(2) || !strings.HasPrefix(conv["runner_ref"].(string), "s1r/") || conv["conversation_id"] != convID {
		t.Fatalf("after relaunch = %d %v", status, conv)
	}
	if got, _ := st.Conversation(ctx, tether.ID); got.Generation != 2 {
		t.Fatalf("Tether generation = %d", got.Generation)
	}

	// The old attachments are stale: 409 stale_attachment with the current generation, on a route and on a stream.
	status, body := c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/lease", map[string]string{"attachment": ctl})
	if status != http.StatusConflict || body["code"] != "stale_attachment" || body["generation"] != float64(2) {
		t.Fatalf("stale lease = %d %v", status, body)
	}
	status, body = c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/turns", map[string]string{"attachment": ctl, "text": "again"})
	if status != http.StatusConflict || body["code"] != "stale_attachment" {
		t.Fatalf("stale submit = %d %v", status, body)
	}
	if _, status := c.stream(convID, viewer, "0"); status != http.StatusConflict {
		t.Fatalf("stale stream = %d", status)
	}

	// Attaching again works, at the new generation, and the next turn runs on the new session.
	ctl2 := attach("controller")
	sc, _ := c.stream(convID, ctl2, strconv.FormatUint(ga[len(ga)-1].ID, 10))
	status, receipt = c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/turns", map[string]string{"attachment": ctl2, "text": "hello again"})
	if status != http.StatusAccepted {
		t.Fatalf("submit after reattach = %d %v", status, receipt)
	}
	until(t, sc, finished)
	if sessions.builds("s1r") != 1 {
		t.Fatalf("new session backend built %d times", sessions.builds("s1r"))
	}
}
