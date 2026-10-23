package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// CW-20261023-0018: a launch file can set lifecycle.binding.
func TestLaunchYAMLLifecycleBinding(t *testing.T) {
	var l Launch
	if err := yaml.Unmarshal([]byte("lifecycle:\n  binding: none\n  idle_timeout: 30m\n"), &l); err != nil {
		t.Fatal(err)
	}
	if l.Lifecycle == nil || l.Lifecycle.BindingMode() != "none" {
		t.Fatalf("lifecycle = %+v, want binding none", l.Lifecycle)
	}
	var unset Launch
	if err := yaml.Unmarshal([]byte("lifecycle:\n  idle_timeout: 30m\n"), &unset); err != nil {
		t.Fatal(err)
	}
	if unset.Lifecycle.BindingMode() != "replace" {
		t.Fatalf("unset binding = %q, want replace", unset.Lifecycle.BindingMode())
	}
}
