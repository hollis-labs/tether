package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveMCPProxyConfigOnlyRequiresProxy(t *testing.T) {
	_, _, err := resolveMCPProxyConfig(false, false, "", "clockwork", "", true)
	if err == nil || !strings.Contains(err.Error(), "--only requires --proxy") {
		t.Fatalf("expected --only requires --proxy error, got %v", err)
	}
}

func TestResolveMCPProxyConfigOnlyRequiresServerList(t *testing.T) {
	_, _, err := resolveMCPProxyConfig(true, false, "", " , ", "", true)
	if err == nil || !strings.Contains(err.Error(), "--only requires a non-empty") {
		t.Fatalf("expected non-empty --only list error, got %v", err)
	}

	_, _, err = resolveMCPProxyConfig(true, false, "", "", "vanta", true)
	if err == nil || !strings.Contains(err.Error(), "--only requires a non-empty") {
		t.Fatalf("expected --only to ignore env fallback, got %v", err)
	}
}

func TestResolveMCPProxyConfigOnlyRejectsConflicts(t *testing.T) {
	_, _, err := resolveMCPProxyConfig(true, true, "", "clockwork", "", true)
	if err == nil || !strings.Contains(err.Error(), "--only cannot be combined with --broker") {
		t.Fatalf("expected --only/--broker conflict, got %v", err)
	}

	_, _, err = resolveMCPProxyConfig(true, false, "vanta", "clockwork", "", true)
	if err == nil || !strings.Contains(err.Error(), "--only cannot be combined with --servers") {
		t.Fatalf("expected --only/--servers conflict, got %v", err)
	}
}

func TestResolveMCPProxyConfigServersFallbackAndOnly(t *testing.T) {
	filter, only, err := resolveMCPProxyConfig(true, false, "", "", "vanta, clockwork", false)
	if err != nil {
		t.Fatalf("resolve servers env: %v", err)
	}
	if only {
		t.Fatal("expected --servers/env mode, got only mode")
	}
	if want := []string{"vanta", "clockwork"}; !reflect.DeepEqual(filter, want) {
		t.Fatalf("env filter = %v, want %v", filter, want)
	}

	filter, only, err = resolveMCPProxyConfig(true, false, "", "cerberus,clockwork", "vanta", true)
	if err != nil {
		t.Fatalf("resolve only: %v", err)
	}
	if !only {
		t.Fatal("expected only mode")
	}
	if want := []string{"cerberus", "clockwork"}; !reflect.DeepEqual(filter, want) {
		t.Fatalf("only filter = %v, want %v", filter, want)
	}
}
