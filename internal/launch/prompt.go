package launch

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/chrispian/agent-mux/internal/config"
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
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		parts = append(parts, strings.TrimRight(string(b), "\n"))
	}
	return strings.Join(parts, "\n\n") + "\n", nil
}
