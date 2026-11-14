package environmentstream

import (
	"encoding/json"
	"net/url"
	"testing"

	mesh "github.com/hollis-labs/libs/mesh/core"
)

// summary is top-level and cell sits inside scope on the wire (CW-20261114-0019).
func TestEventWireCarriesSummaryAndScopeCell(t *testing.T) {
	e := Event{EnvironmentID: "env", Seq: 7, Summary: "@photon-tether read request from @archie", Cell: "photon",
		Event: mesh.Event{Kind: "message.read", Scope: &mesh.EventScope{TaskID: "CW-1"}}}
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
	if back.Cell != "photon" || back.Summary != e.Summary || back.Seq != 7 || back.Scope == nil || back.Scope.TaskID != "CW-1" {
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
	if !f.Match(Event{Cell: "photon"}) || f.Match(Event{Cell: "command"}) || f.Match(Event{}) {
		t.Fatal("scope.cell filter mismatch")
	}
}
