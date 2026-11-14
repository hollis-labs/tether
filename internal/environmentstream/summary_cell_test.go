package environmentstream

import (
	"encoding/json"
	"net/url"
	"testing"

	mesh "github.com/hollis-labs/libs/mesh/core"
)

// summary is top-level and cell sits inside scope on the wire (CW-20261114-0019).
func TestEventWireCarriesSummaryAndScopeCell(t *testing.T) {
	e := Event{EnvironmentID: "env", Seq: 7,
		Event: mesh.Event{Kind: "message.read", Summary: "@photon-tether read request from @archie",
			Scope: &mesh.EventScope{TaskID: "CW-1", Cell: "photon"}}}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Summary string `json:"summary"`
		Seq     int64  `json:"seq"`
		Kind    string `json:"kind"`
		Scope   struct {
			Cell   string `json:"cell"`
			TaskID string `json:"task_id"`
		} `json:"scope"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Summary != e.Summary || wire.Scope.Cell != "photon" || wire.Scope.TaskID != "CW-1" || wire.Seq != 7 || wire.Kind != "message.read" {
		t.Fatalf("wire = %s", raw)
	}
	var back Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Scope == nil || back.Scope.Cell != "photon" || back.Summary != e.Summary || back.Seq != 7 || back.Scope.TaskID != "CW-1" {
		t.Fatalf("round trip = %+v", back)
	}
	// No cell, no scope block: nothing is invented.
	plain, _ := json.Marshal(Event{EnvironmentID: "env", Seq: 1})
	var obj map[string]any
	_ = json.Unmarshal(plain, &obj)
	if _, ok := obj["scope"]; ok {
		t.Fatalf("scope invented: %s", plain)
	}
}

func TestFilterByCell(t *testing.T) {
	f, err := ParseFilter(url.Values{"scope.cell": {"photon"}})
	if err != nil {
		t.Fatal(err)
	}
	cell := func(c string) Event { return Event{Event: mesh.Event{Scope: &mesh.EventScope{Cell: c}}} }
	if !f.Match(cell("photon")) || f.Match(cell("command")) || f.Match(Event{}) || f.Match(cell("")) {
		t.Fatal("scope.cell filter mismatch")
	}
}

// A stored row's summary and cell reach the projected mesh event, and from
// there the wire, without a projection-side shim.
func TestMapStoredCarriesSummaryAndCell(t *testing.T) {
	e := storedExample()
	e.EventID = "environment:42"
	e.Envelope.Summary = "task CW-1 moved to running"
	e.Envelope.Dims.Cell = "photon"
	out, err := (StoredIDProjection{Cutover: 42}).MapStored("environment", e)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Summary string `json:"summary"`
		Scope   struct {
			Cell string `json:"cell"`
		} `json:"scope"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || wire.Summary != e.Envelope.Summary || wire.Scope.Cell != "photon" {
		t.Fatalf("wire = %s (%v)", raw, err)
	}
}
