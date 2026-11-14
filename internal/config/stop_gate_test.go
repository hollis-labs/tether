package config

import "testing"

func TestEffectiveStopGate(t *testing.T) {
	on, off := true, false
	var g Global
	if EffectiveStopGate(g, Agent{}) {
		t.Fatal("default must be off")
	}
	if !EffectiveStopGate(g, Agent{StopGate: &on}) {
		t.Fatal("agent opt-in ignored")
	}
	g.Catalog.Defaults.StopGate = true
	if !EffectiveStopGate(g, Agent{}) || EffectiveStopGate(g, Agent{StopGate: &off}) {
		t.Fatal("fleet default or per-agent opt-out ignored")
	}
}
