package workerruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
)

type failedObservationWriter struct{}

func (failedObservationWriter) Write([]byte) (int, error) { return 0, errors.New("disconnected") }

func TestWorkerPermissionObservesActualReplyAndTerminal(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "replied", true: "disconnected"}[failed], func(t *testing.T) {
			var reply, output bytes.Buffer
			rpc := &providerRPC{writer: &reply, pending: map[int64]chan rpcReply{}, done: make(chan struct{}), completed: make(chan struct{}, 1)}
			if failed {
				rpc.writer = failedObservationWriter{}
			}
			rpc.read(strings.NewReader(`{"id":9007199254740993,"method":"item/tool/requestUserInput","params":{"turnId":"turn-A","secret":"private-question"}}
{"id":"unknown","method":"unsupported/request","params":{"turnId":"turn-A"}}
{"method":"turn/completed","params":{"turn":{"id":"turn-A","status":"completed"}}}
`), &output)
			var observations []runtimeevents.Event
			for _, line := range bytes.Split(output.Bytes(), []byte("\n")) {
				var v struct {
					Type  string              `json:"type"`
					Event runtimeevents.Event `json:"event"`
				}
				if json.Unmarshal(line, &v) == nil && v.Type == "tether.runtime_event" {
					if bytes.Contains(line, []byte("private-question")) || bytes.Contains(line, []byte("secret")) {
						t.Fatal("observation leaked params")
					}
					observations = append(observations, v.Event)
				}
			}
			want := 3
			if failed {
				want = 1
			}
			if len(observations) != want || observations[0].Kind != runtimeevents.KindAgentPermissionRequested || observations[0].TurnID != "turn-A" || !bytes.Contains(observations[0].Payload, []byte("9007199254740993")) {
				t.Fatalf("observations %+v", observations)
			}
			if !failed {
				if observations[1].Kind != runtimeevents.KindAgentPermissionResolved || !bytes.Contains(observations[1].Payload, []byte(`"allowed":false`)) || observations[2].Kind != runtimeevents.KindTurnCompleted {
					t.Fatalf("resolution %+v", observations)
				}
				if !strings.Contains(reply.String(), `"code":-32601`) || !strings.Contains(reply.String(), `"id":9007199254740993`) {
					t.Fatalf("existing fallback reply changed: %s", reply.String())
				}
			}
		})
	}
}
