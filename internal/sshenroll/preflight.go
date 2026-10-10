package sshenroll

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

type Preflight struct {
	OS                                                            string `json:"os"`
	Arch                                                          string `json:"arch"`
	Home                                                          string `json:"home"`
	Path                                                          string `json:"login_path"`
	Git, Bubblewrap, UserManager, Linger, Writable, ExistingState bool
	Providers                                                     map[string]string `json:"providers"`
}

func (p Preflight) Validate(providers []string) error {
	if p.OS != "Linux" || (p.Arch != "amd64" && p.Arch != "arm64") {
		return problem("preflight", "unsupported-platform", "Only Linux x86_64/amd64 and aarch64/arm64 workers are supported.")
	}
	if !filepath.IsAbs(p.Home) || p.Home == "/" || p.Path == "" || strings.ContainsAny(p.Home+p.Path, "\r\n\x00\t") {
		return problem("preflight", "invalid-login-environment", "Prepare HOME and PATH in a non-interactive login shell.")
	}
	checks := []struct {
		ok         bool
		code, hint string
	}{{p.Git, "git-missing", "Install git on the worker."}, {p.Bubblewrap, "bubblewrap-unusable", "Install bubblewrap and enable an unprivileged user namespace for the worker account."}, {p.UserManager, "user-manager-unavailable", "Prepare a working systemd --user login session."}, {p.Linger, "linger-disabled", "An administrator must run loginctl enable-linger for this worker account; enrollment never invokes sudo."}, {p.Writable, "state-unwritable", "Make the dedicated worker home writable without running enrollment as root."}}
	for _, c := range checks {
		if !c.ok {
			return problem("preflight", c.code, c.hint)
		}
	}
	for _, name := range providers {
		if !filepath.IsAbs(p.Providers[name]) {
			return problem("preflight", "provider-path-missing", "Install and separately log in each selected provider, and put its CLI on the non-interactive login PATH.")
		}
	}
	return nil
}

func preflightScript(providers []string) string {
	s := `set -eu
printf 'os\t%s\n' "$(uname -s)"
case "$(uname -m)" in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; *) arch=unsupported;; esac
printf 'arch\t%s\nhome\t%s\npath\t%s\n' "$arch" "$HOME" "$PATH"
probe() { if "$@" >/dev/null 2>&1; then printf yes; else printf no; fi; }
printf 'git\t%s\n' "$(probe command -v git)"
printf 'bubblewrap\t%s\n' "$(probe bwrap --ro-bind / / --unshare-user -- /bin/true)"
printf 'manager\t%s\n' "$(probe systemctl --user show-environment)"
linger=$(loginctl show-user "$(id -u)" --property=Linger --value 2>/dev/null || true)
printf 'linger\t%s\n' "$linger"
if [ "$(id -u)" != 0 ] && [ -d "$HOME" ] && [ -w "$HOME" ] && [ ! -L "$HOME/.tether" ] && { [ ! -e "$HOME/.tether" ] || { [ -d "$HOME/.tether" ] && [ -w "$HOME/.tether" ]; }; }; then writable=yes; else writable=no; fi
printf 'writable\t%s\n' "$writable"
if [ -e "$HOME/.tether/catalog/global.yaml" ] || [ -e "$HOME/.tether/run/tetherd.pid" ] || [ -e "$HOME/.tether/runtime/ownership" ] || [ -e "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/tether-worker.service" ]; then existing=yes; else existing=no; fi
printf 'existing\t%s\n' "$existing"
`
	for _, name := range providers {
		s += fmt.Sprintf("printf 'provider:%s\\t%%s\\n' \"$(command -v %s 2>/dev/null || true)\"\n", name, name)
	}
	return s
}
func parsePreflight(data []byte) (Preflight, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		k, v, ok := strings.Cut(line, "\t")
		if !ok || fields[k] != "" {
			return Preflight{}, problem("preflight", "invalid-output", "The non-interactive login shell must not print banners or malformed inventory.")
		}
		fields[k] = v
	}
	p := Preflight{OS: fields["os"], Arch: fields["arch"], Home: fields["home"], Path: fields["path"], Git: fields["git"] == "yes", Bubblewrap: fields["bubblewrap"] == "yes", UserManager: fields["manager"] == "yes", Linger: fields["linger"] == "yes", Writable: fields["writable"] == "yes", ExistingState: fields["existing"] == "yes", Providers: map[string]string{}}
	for k, v := range fields {
		if strings.HasPrefix(k, "provider:") {
			p.Providers[strings.TrimPrefix(k, "provider:")] = v
		}
	}
	return p, nil
}
func (s *SSH) Preflight(ctx context.Context, o Options) (Preflight, error) {
	if err := o.validate(); err != nil {
		return Preflight{}, err
	}
	data, err := s.run(ctx, preflightScript(o.Providers), nil, s.StepTimeout)
	if err != nil {
		return Preflight{}, safeStep("preflight", err)
	}
	return parsePreflight(data)
}
