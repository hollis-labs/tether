package workerruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/substrate/harness/shim"
	"time"
)

func (e *Executor) submit(ctx context.Context, s *execution, key, kind string, body any) error {
	if key == "" {
		return fmt.Errorf("command identity required")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ch := make(chan shim.Frame, 1)
	s.mu.Lock()
	s.pending[key] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, key); s.mu.Unlock() }()
	if err := s.client.SendFrame(shim.Frame{Major: shim.ProtocolMajor, Minor: shim.ProtocolMinor, Type: kind, RequestID: key, Session: s.ID, Epoch: s.client.Epoch, Body: raw}); err != nil {
		return err
	}
	select {
	case reply := <-ch:
		if reply.Type == "error" {
			var problem struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(reply.Body, &problem)
			return fmt.Errorf("shim command refused: %s", problem.Code)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return fmt.Errorf("shim command timeout")
	}
}
