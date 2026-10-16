//go:build !windows

package shimcodex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func backlogProjectionState(count int) State {
	p := projectionFixture()
	s := State{Version: Version, Binding: p.Binding, Revision: 1, Epoch: 1, NextID: FirstID, Cursor: "j:1"}
	body := []byte(`{"method":"account/updated","params":{}}`)
	for range count {
		start := s.StreamOffset
		s.StreamOffset += uint64(len(body) + 1)
		s.Inbox = append(s.Inbox, Event{Identity: fmt.Sprintf("j:stdout:%d:%d", start, s.StreamOffset), Cursor: s.Cursor, Raw: append(json.RawMessage(nil), body...)})
	}
	s.PartialStart = s.StreamOffset
	return s
}

func TestDeliveryProjectionBacklogWithinDeliveryBound(t *testing.T) {
	for _, count := range []int{977, ProjectionSources} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			state := backlogProjectionState(count)
			before, _ := json.Marshal(state)
			ctx, cancel := bounded(context.Background())
			defer cancel()
			start := time.Now()
			p, err := BuildDeliveryProjection(state)
			t.Logf("frozen %d-record projection took %s", count, time.Since(start))
			if err != nil || ctx.Err() != nil {
				t.Fatal("retained batch cannot finish within the controller delivery bound", err, ctx.Err())
			}
			if len(p.Sources) != count || p.AcceptedSourceCursor != state.Cursor || p.StdoutOffset != state.StreamOffset || p.DeliveredHighWater != "" || len(p.Turns) != 0 || p.Terminal != nil {
				t.Fatal("batch lost source coverage or fabricated public output")
			}
			after, _ := json.Marshal(state)
			if !bytes.Equal(before, after) {
				t.Fatal("projection changed private inbox bytes")
			}
		})
	}
}

func TestDeliveryProjectionBacklogRefusesWithoutChangingObligations(t *testing.T) {
	for _, kind := range []string{"capacity", "unsupported", "splice", "gap"} {
		t.Run(kind, func(t *testing.T) {
			state := backlogProjectionState(ProjectionSources)
			code := ""
			switch kind {
			case "capacity":
				state = backlogProjectionState(ProjectionSources + 1)
				code = "pressure_retained"
			case "unsupported":
				last := &state.Inbox[len(state.Inbox)-1]
				start := state.StreamOffset - uint64(len(last.Raw)+1)
				last.Raw = []byte(`{"method":"future/output","params":{}}`)
				state.StreamOffset = start + uint64(len(last.Raw)+1)
				state.PartialStart = state.StreamOffset
				last.Identity = fmt.Sprintf("j:stdout:%d:%d", start, state.StreamOffset)
				code = "output_unsupported"
			case "splice":
				last := &state.Inbox[len(state.Inbox)-1]
				last.Identity = state.Inbox[0].Identity
				last.Raw = []byte(`{"method":"account/updated","params":{"changed":true}}`)
				code = "source_conflict"
			case "gap":
				state.Inbox[len(state.Inbox)-1].Cursor = "j:3"
				code = "source_gap"
			}
			before, _ := json.Marshal(state)
			if _, err := BuildDeliveryProjection(state); !HasCode(err, code) {
				t.Fatal("retained batch was not refused", kind, err)
			}
			after, _ := json.Marshal(state)
			if !bytes.Equal(before, after) || state.Delivery != nil {
				t.Fatal("refused batch changed private obligations or minted delivery")
			}
		})
	}
}
