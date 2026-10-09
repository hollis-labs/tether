package launchartifacts

import (
	"os"
	"path/filepath"
)

// ValidateNativeLink checks an already planted native home against the trusted
// pre-redirect credential capture. It neither reads credentials nor plants or
// changes the old home. A redirected home is never a credential source.
func (h *CodexHome) ValidateNativeLink(nativeRoot string) error {
	if err := h.validate(); err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(nativeRoot)
	if err != nil || !filepath.IsAbs(nativeRoot) || canonical != nativeRoot {
		return refusal("codex_native_home_unavailable")
	}
	path := filepath.Join(nativeRoot, "auth.json")
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return refusal("codex_native_credential_mapping_unavailable")
	}
	target, err := os.Readlink(path)
	if err != nil || target != filepath.Join(h.path, "auth.json") {
		return refusal("codex_native_credential_mapping_changed")
	}
	return h.validate()
}
