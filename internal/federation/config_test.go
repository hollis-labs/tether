package federation

import "testing"

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "disabled zero value is a legal standalone install",
			cfg:  Config{},
		},
		{
			name: "disabled block ignores otherwise-bad fields",
			cfg:  Config{Enabled: false, LocalAuthority: "", Peers: []Peer{{Authority: "/bad"}}},
		},
		{
			name: "enabled peerless install",
			cfg:  Config{Enabled: true, LocalAuthority: "tether"},
		},
		{
			name: "enabled with a well-formed peer",
			cfg: Config{
				Enabled:        true,
				LocalAuthority: "tether",
				Peers:          []Peer{{Authority: "torque", BaseURL: "http://10.0.0.4:7777"}},
			},
		},
		{
			name:    "enabled requires a local authority",
			cfg:     Config{Enabled: true},
			wantErr: true,
		},
		{
			name:    "local authority rejects a slash",
			cfg:     Config{Enabled: true, LocalAuthority: "bad/seg"},
			wantErr: true,
		},
		{
			name:    "local authority rejects whitespace",
			cfg:     Config{Enabled: true, LocalAuthority: "bad seg"},
			wantErr: true,
		},
		{
			name: "peer authority is required",
			cfg: Config{
				Enabled:        true,
				LocalAuthority: "tether",
				Peers:          []Peer{{BaseURL: "http://host"}},
			},
			wantErr: true,
		},
		{
			name: "peer base_url is required",
			cfg: Config{
				Enabled:        true,
				LocalAuthority: "tether",
				Peers:          []Peer{{Authority: "torque"}},
			},
			wantErr: true,
		},
		{
			name: "peer base_url must be http(s)",
			cfg: Config{
				Enabled:        true,
				LocalAuthority: "tether",
				Peers:          []Peer{{Authority: "torque", BaseURL: "ftp://host"}},
			},
			wantErr: true,
		},
		{
			name: "peer base_url must include a host",
			cfg: Config{
				Enabled:        true,
				LocalAuthority: "tether",
				Peers:          []Peer{{Authority: "torque", BaseURL: "http://"}},
			},
			wantErr: true,
		},
		{
			name: "a peer may not duplicate the local authority",
			cfg: Config{
				Enabled:        true,
				LocalAuthority: "tether",
				Peers:          []Peer{{Authority: "tether", BaseURL: "http://host"}},
			},
			wantErr: true,
		},
		{
			name: "a peer authority may not be registered twice",
			cfg: Config{
				Enabled:        true,
				LocalAuthority: "tether",
				Peers: []Peer{
					{Authority: "torque", BaseURL: "http://a"},
					{Authority: "torque", BaseURL: "http://b"},
				},
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}
