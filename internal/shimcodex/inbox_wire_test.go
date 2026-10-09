//go:build !windows

package shimcodex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestInboxWirePreservesNativeSpellingThroughCommitAndEpoch(t *testing.T) {
	e, store := newEngine(t)
	// Whitespace, HTML characters, literal U+2028 and an escaped character all
	// survive as native bytes. Semantic JSON equivalence cannot witness spans.
	wire := []byte("{ \"method\" : \"unknown/native\", \"params\" : {\"text\":\"<>&\u2028\\u0061\"} }")
	accepted, err := e.AcceptOutput(context.Background(), 1, "j:1", "stdout", append(append([]byte(nil), wire...), '\n'))
	if err != nil || !accepted {
		t.Fatal("native acceptance", err)
	}
	e, err = Open(context.Background(), store, e.Snapshot().Binding, 2, false, e.limits)
	if err != nil {
		t.Fatal(err)
	}
	s := e.Snapshot()
	if len(s.Inbox) != 1 || s.Inbox[0].LegacyRawJSON || !bytes.Equal(s.Inbox[0].Raw, wire) {
		t.Fatal("native spelling changed in a durable round trip")
	}
	p, err := ProjectFrozenSource(projectionFixture(), s.Inbox[0])
	if err != nil || p.StdoutOffset != uint64(len(wire)+1) || p.Sources[0].PayloadSHA256 != digest(wire) {
		t.Fatal("physical span/hash changed", err)
	}
	if p.DeliveredHighWater != "" || p.Sources[0].Disposition != "retained_unsupported" {
		t.Fatal("byte preservation issued delivery")
	}
}

func TestLegacyInboxRequiresWitnessAndKeepsAllOtherObligations(t *testing.T) {
	for _, kind := range []string{"missing", "splice", "cursor", "concurrent", "valid"} {
		t.Run(kind, func(t *testing.T) {
			e, store := newEngine(t)
			wire := []byte(`{ "method":"unknown/native", "params":{"text":">"} }`)
			normal, _ := json.Marshal(json.RawMessage(wire))
			old := []byte(fmt.Sprintf(`{"identity":"j:stdout:0:%d","cursor":"j:1","raw":%s}`, len(wire)+1, normal))
			var event Event
			if err := json.Unmarshal(old, &event); err != nil || !event.LegacyRawJSON {
				t.Fatal("legacy source was treated as a byte witness", err)
			}
			e.mu.Lock()
			err := e.commitLocked(context.Background(), func(s *State) error {
				s.Inbox = []Event{event}
				s.Cursor = "j:1"
				s.StreamOffset = uint64(len(wire) + 1)
				s.PartialStart = s.StreamOffset
				return nil
			})
			e.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			e, err = Open(context.Background(), store, e.Snapshot().Binding, 2, false, e.limits)
			if err != nil {
				t.Fatal(err)
			}
			before := e.Snapshot()
			if _, err := BuildDeliveryProjection(before); !HasCode(err, "legacy_bytes_pending") {
				t.Fatal("legacy source reached public projection", err)
			}
			var read InboxWireReader
			if kind != "missing" {
				read = func(_ context.Context, frozen State) ([]Event, error) {
					frozen.Inbox[0].Raw = append([]byte(nil), wire...)
					frozen.Inbox[0].LegacyRawJSON = false
					switch kind {
					case "splice":
						frozen.Inbox[0].Raw = []byte(`{"method":"other/native"}`)
					case "cursor":
						frozen.Inbox[0].Cursor = "j:2"
					case "concurrent":
						if err := e.AcceptReplayHighWater(context.Background(), e.Snapshot().Epoch, "j:1"); err != nil {
							t.Fatal(err)
						}
					}
					return frozen.Inbox, nil
				}
			}
			err = e.RestoreInboxWire(context.Background(), read)
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				after := e.Snapshot()
				if !bytes.Equal(after.Inbox[0].Raw, wire) || after.Inbox[0].LegacyRawJSON || after.Revision != before.Revision+1 {
					t.Fatal("witness was not committed")
				}
				after.Revision = before.Revision
				after.Inbox = before.Inbox
				a, _ := json.Marshal(before)
				b, _ := json.Marshal(after)
				if !bytes.Equal(a, b) {
					t.Fatal("restoration changed another protocol obligation")
				}
			} else if err == nil || !e.Snapshot().Inbox[0].LegacyRawJSON {
				t.Fatal("unverified wire source committed", err)
			}
		})
	}
}
