package teamcli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/hollis-labs/tether/internal/teamspolicy"
	"github.com/spf13/cobra"
)

// PinOf parses an authored agent definition file and returns its exact pin.
// It is offline: it checks the schema and computes the semantic digest, but
// does not resolve pinned content references (the daemon does that at start).
func PinOf(path string) (id, revision, digest string, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // The operator explicitly selected this local definition file.
	if err != nil {
		return "", "", "", err
	}
	id, revision, digest, err = teamspolicy.PinOfBytes(raw)
	if err != nil {
		return "", "", "", fmt.Errorf("%s: %w", path, err)
	}
	return id, revision, digest, nil
}

func addPinCommand(parent *cobra.Command) {
	parent.AddCommand(&cobra.Command{
		Use:   "pin FILE...",
		Short: "Print the id, revision and digest pin of agent definition files (offline)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			enc := json.NewEncoder(cmd.OutOrStdout())
			for _, path := range args {
				id, rev, digest, err := PinOf(path)
				if err != nil {
					return err
				}
				if err := enc.Encode(map[string]string{"file": path, "id": id, "revision": rev, "digest": digest}); err != nil {
					return err
				}
			}
			return nil
		},
	})
}
