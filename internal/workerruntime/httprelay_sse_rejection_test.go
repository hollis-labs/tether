package workerruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPRelayRecoversExplicitSSERejection(t *testing.T) {
	for _, method := range []string{"tools/list", "tools/call"} {
		for _, code := range []string{"mcp_initialize_required", "session_expired"} {
			t.Run(method+"/"+code, func(t *testing.T) {
				var rejected atomic.Bool
				transport := stoppingRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodPost {
						raw, _ := io.ReadAll(r.Body)
						r.Body = io.NopCloser(bytes.NewReader(raw))
						if methodName(raw) == method && rejected.CompareAndSwap(false, true) {
							payload := fmt.Sprintf("data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"error\":{\"code\":-32000,\"message\":%q}}\n\n", requestID(raw), code)
							return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
						}
					}
					return http.DefaultTransport.RoundTrip(r)
				})
				hub := &fakeHub{}
				rig := newRelayRig(t, hub, transport)
				rig.handshake()
				rig.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":%q,"params":{"name":"write"}}`, method))
				response := rig.recvReply()
				if response["id"] != float64(2) || response["error"] != nil || response["result"] == nil {
					t.Fatalf("SSE rejection leaked: %v", response)
				}
				if hub.count("initialize") != 2 || hub.count(method) != 1 {
					t.Fatalf("initializes=%d dispatches=%d", hub.count("initialize"), hub.count(method))
				}
				rig.finishDrainingNotifications()
			})
		}
	}
}

func TestHTTPRelaySSECannotReplayAfterTerminalResponse(t *testing.T) {
	var output bytes.Buffer
	relay := &HTTPRelay{out: &output}
	stream := "data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"content\":[]}}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"error\":{\"code\":-32000,\"message\":\"mcp_initialize_required\"}}\n\n"
	if code := relay.readSSECall(strings.NewReader(stream), json.RawMessage(`2`)); code != "" {
		t.Fatalf("completed tool allowed retry: %s", code)
	}
	if !strings.Contains(output.String(), `"result"`) {
		t.Fatal("terminal result lost")
	}
}

func TestHTTPRelaySSERejectionRequiresMatchingRequest(t *testing.T) {
	for _, fixture := range []struct {
		payload string
		id      json.RawMessage
	}{
		{`{"jsonrpc":"2.0","id":99,"error":{"code":-32000,"message":"mcp_initialize_required"}}`, json.RawMessage(`2`)},
		{`{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"session_expired"}}`, nil},
		{`{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"generic_failure"}}`, json.RawMessage(`2`)},
	} {
		var output bytes.Buffer
		relay := &HTTPRelay{out: &output}
		if code := relay.readSSECall(strings.NewReader("data: "+fixture.payload+"\n\n"), fixture.id); code != "" {
			t.Fatalf("unrelated/generic response allowed retry: %s", code)
		}
		if !strings.Contains(output.String(), fixture.payload) {
			t.Fatal("unrelated/generic error swallowed")
		}
	}
}

// Keep the downstream reader alive through relay shutdown, as a connected MCP
// client does. A late standalone notification can follow the final call reply;
// stopping reads first leaves emit blocked on the rig's unbuffered stdout pipe.
func (g *relayRig) finishDrainingNotifications() {
	g.t.Helper()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for g.out.Scan() {
		}
	}()
	g.finish()
	<-drained // Run closes stdout before reporting completion.
	if err := g.out.Err(); err != nil {
		g.t.Fatal(err)
	}
}

func TestHTTPRelayShutdownWithNotificationAfterFinalReply(t *testing.T) {
	allowNotification := make(chan struct{})
	getStarted := make(chan struct{})
	transport := stoppingRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			close(getStarted)
			select {
			case <-allowNotification:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return http.DefaultTransport.RoundTrip(req)
	})
	rig := newRelayRig(t, &fakeHub{}, transport)
	rig.handshake()
	<-getStarted
	rig.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write"}}`)
	if response := rig.recvReply(); response["result"] == nil {
		t.Fatalf("reply=%v", response)
	}
	// Force the ordering seen in CI: no notification is available while reading
	// the terminal reply, but one can arrive during shutdown.
	close(allowNotification)
	rig.finishDrainingNotifications()
}
