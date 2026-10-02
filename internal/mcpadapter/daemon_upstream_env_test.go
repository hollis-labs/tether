package mcpadapter

import (
	"strings"
	"testing"
)

func TestDaemonUpstreamEnvironment_ExplicitDelegationOnly(t *testing.T) {
	inherited := []string{"PATH=/system/bin", "HOME=/temporary/home", "LANG=C.UTF-8", "LC_TIME=C", "TMPDIR=/temporary/tmp", "TETHER_TOKEN=daemon-bearer", "OTHER_APP_API_KEY=daemon-secret", "LD_PRELOAD=/injected.so", "DBUS_SESSION_BUS_ADDRESS=host-bus", "LC_FAKE_SECRET=not-a-locale"}
	env := map[string]string{}
	for _, value := range daemonUpstreamEnvironment(inherited, map[string]string{"PATH": "/catalog/bin", "UPSTREAM_TOKEN": "explicit-credential"}) {
		name, value, _ := strings.Cut(value, "=")
		env[name] = value
	}
	for _, name := range []string{"TETHER_TOKEN", "OTHER_APP_API_KEY", "LD_PRELOAD", "DBUS_SESSION_BUS_ADDRESS", "LC_FAKE_SECRET"} {
		if _, present := env[name]; present {
			t.Errorf("inherited private daemon setting %s", name)
		}
	}
	for name, want := range map[string]string{"PATH": "/catalog/bin", "HOME": "/temporary/home", "LANG": "C.UTF-8", "LC_TIME": "C", "TMPDIR": "/temporary/tmp", "UPSTREAM_TOKEN": "explicit-credential"} {
		if env[name] != want {
			t.Errorf("missing portable or explicitly configured setting %s", name)
		}
	}
}
