package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagedLaunchCapabilityRequiresProvenance(t *testing.T) {
	f := newFakeManager(t)
	f.install(t, "0.8.0")
	executable := filepath.Join(f.manager.Runtime.Root, "versions", "0.8.0", "tether")
	if capability := LaunchUpdateCapability(f.manager.Runtime.Root, executable); capability != "service" {
		t.Fatal(capability)
	}
	if capability := LaunchUpdateCapability("", executable); capability != "foreground" {
		t.Fatal(capability)
	}
	if capability := LaunchUpdateCapability(f.manager.Runtime.Root, "/unverified/tether"); capability != "none" {
		t.Fatal("unverified executable advertised service")
	}
	if err := os.WriteFile(executable, []byte("changed"), 0755); err != nil {
		t.Fatal(err)
	}
	if capability := LaunchUpdateCapability(f.manager.Runtime.Root, executable); capability != "none" {
		t.Fatal("changed executable advertised service")
	}
}

func TestManagedLaunchCapabilityRefusesMarkerAlone(t *testing.T) {
	r := testRuntime(t)
	if err := r.prepare(); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(r.Root, "ownership"), []byte("managed\n")); err != nil {
		t.Fatal(err)
	}
	if capability := LaunchUpdateCapability(r.Root, filepath.Join(r.Root, "current", "tether")); capability != "none" {
		t.Fatal("marker alone advertised service")
	}
}
