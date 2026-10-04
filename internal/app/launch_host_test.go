package app

import (
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

func TestResolveLaunchHost(t *testing.T) {
	for _, tc := range []struct {
		env, catalog string
		want         LaunchHost
	}{
		{"", "", HostDirect},
		{"", "shim", HostShim},
		{"", "unexpected", HostDirect},
		{"shim", "direct", HostShim},
		{"direct", "shim", HostDirect},
		{"unexpected", "shim", HostDirect},
		{"SHIM", "shim", HostDirect},
	} {
		t.Run(tc.env+"/"+tc.catalog, func(t *testing.T) {
			t.Setenv(EnvLaunchHost, tc.env)
			cat := &config.Catalog{}
			cat.Global.Catalog.Defaults.LaunchHost = tc.catalog
			if got := resolveLaunchHost(cat); got != tc.want {
				t.Fatalf("host = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLaunchHostRereadsOverride(t *testing.T) {
	svc := &Service{}
	t.Setenv(EnvLaunchHost, "")
	if svc.LaunchHost() != HostDirect {
		t.Fatal("nil catalog changed the default")
	}
	t.Setenv(EnvLaunchHost, "shim")
	if svc.LaunchHost() != HostShim {
		t.Fatal("override was cached")
	}
	t.Setenv(EnvLaunchHost, "direct")
	if svc.LaunchHost() != HostDirect {
		t.Fatal("override was cached")
	}
}
