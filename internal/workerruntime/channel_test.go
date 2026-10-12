package workerruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func TestOutpostChannelReaderDuringLaunch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := make(chan struct{})
	defer close(blocked)
	pong := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		var f workerwire.Frame
		if ws.ReadJSON(&f) != nil {
			return
		}
		_ = ws.WriteJSON(workerwire.Frame{T: "welcome", Features: []string{"bind", "deliver"}})
		_ = ws.WriteJSON(workerwire.Frame{T: "launch", CmdID: "slow-launch"})
		_ = ws.WriteJSON(workerwire.Frame{T: "stop", CmdID: "stop-during-launch"})
		_ = ws.WriteJSON(workerwire.Frame{T: "ping", TS: 123})
		_ = ws.SetReadDeadline(time.Now().Add(time.Second))
		seenPong, seenStop := false, false
		for !seenPong || !seenStop {
			if ws.ReadJSON(&f) != nil {
				break
			}
			seenPong = seenPong || (f.T == "pong" && f.TS == 123)
			seenStop = seenStop || (f.T == "state" && f.CmdID == "stop-during-launch")
		}
		pong <- seenPong && seenStop
		<-ctx.Done()
	}))
	defer func() { cancel(); server.Close() }()
	dir := t.TempDir()
	token, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "device.token"), []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Root: dir, Config: Config{Hub: server.URL, Label: "probe"}, Caps: func(context.Context) json.RawMessage { return json.RawMessage(`{}`) }, Command: func(ctx context.Context, f workerwire.Frame) error {
		if f.T == "launch" {
			select {
			case <-blocked:
			case <-ctx.Done():
			}
		}
		return nil
	}}
	handle := w.Command
	w.Command = func(ctx context.Context, f workerwire.Frame) error {
		if f.T == "stop" {
			return w.Send(workerwire.Frame{T: "state", CmdID: f.CmdID})
		}
		return handle(ctx, f)
	}
	go func() { _ = w.Run(ctx) }()
	select {
	case ok := <-pong:
		if !ok {
			t.Fatal("slow launch blocked reader/pong or stop handling")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader timeout")
	}
	cancel()
}

func TestOutpostReplayBatches(t *testing.T) {
	s := &execution{ID: "s", State: "idle"}
	for i := int64(1); i <= 300; i++ {
		s.outputs = append(s.outputs, outputRecord{Frame: workerwire.Frame{T: "output", Seq: i, Data: make([]byte, 8192)}})
	}
	var batch []workerwire.Frame
	e := &Executor{sessions: map[string]*execution{"s": s}, Send: func(f workerwire.Frame) error { batch = append(batch, f); return nil }}
	if err := e.replay("s", 1); err != nil {
		t.Fatal(err)
	}
	count, bytes := 0, 0
	for _, f := range batch {
		if f.T == "output" {
			count++
			bytes += len(f.Data)
		}
	}
	last := batch[len(batch)-1]
	if count > 256 || bytes > 1<<20 || last.T != "replay_end" || last.FromSeq != int64(count)+1 {
		t.Fatalf("unbounded/bad continuation: frames=%d bytes=%d end=%+v", count, bytes, last)
	}
	batch = nil
	if err := e.replay("s", last.FromSeq); err != nil {
		t.Fatal(err)
	}
	if batch[0].Seq != last.FromSeq {
		t.Fatal("continuation lost output")
	}
}
