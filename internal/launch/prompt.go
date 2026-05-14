package launch

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/tether/internal/config"
)

// Compose reads each fragment path (relative paths are resolved against catalogRoot)
// and concatenates with blank-line separators.
func Compose(catalogRoot string, fragments []string) (string, error) {
	var parts []string
	for _, frag := range fragments {
		p := frag
		if !filepath.IsAbs(p) && !strings.HasPrefix(p, "~") {
			p = filepath.Join(catalogRoot, p)
		}
		p = config.Expand(p)
		// Prompt fragments are catalog-sourced; trusted operator input.
		b, err := os.ReadFile(p) //nolint:gosec // G304: catalog-sourced path

		if err != nil {
			return "", err
		}
		parts = append(parts, strings.TrimRight(string(b), "\n"))
	}
	return strings.Join(parts, "\n\n") + "\n", nil
}
