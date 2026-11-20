package gatewaylaunch

import (
	"path/filepath"
	"slices"

	"github.com/hollis-labs/libs/harness/core/sandbox"
)

// ReadableProfile exposes only this session's private credential directory to a
// narrowed filesystem profile. Mount the directory rather than the file, so
// the relay observes atomic credential replacement. Existing denies still win.
// Host-filesystem and unwrapped profiles already see the launcher namespace.
func ReadableProfile(p sandbox.Profile, tokenPath string) sandbox.Profile {
	if p.ID == "" || p.HostFilesystem {
		return p
	}
	p.FS.Read = append(slices.Clone(p.FS.Read), filepath.Dir(tokenPath))
	return p
}
