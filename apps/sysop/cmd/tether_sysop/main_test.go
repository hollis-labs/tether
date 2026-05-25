package main

import "testing"

func TestStripRegistryURN(t *testing.T) {
	src := []byte("kind: cerberus-project/v1\nowner: tether\nregistry_urn: msg://agent/agent-mux/prj_test\nproject:\n  id: tether\n")
	got := string(stripRegistryURN(src))
	if got != "kind: cerberus-project/v1\nowner: tether\nproject:\n  id: tether\n" {
		t.Fatalf("stripRegistryURN mismatch:\n%s", got)
	}
}

func TestShouldRetryCerberusTetherRegister(t *testing.T) {
	if !shouldRetryCerberusTetherRegister("tether-daemon-service", errString("resource \"tether-daemon-service\" not found")) {
		t.Fatalf("expected retry for tether resource not found")
	}
	if shouldRetryCerberusTetherRegister("nanite-api-service", errString("resource \"nanite-api-service\" not found")) {
		t.Fatalf("did not expect retry for non-tether resource")
	}
	if shouldRetryCerberusTetherRegister("tether-daemon-service", errString("permission denied")) {
		t.Fatalf("did not expect retry for unrelated error")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
