//go:build !windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hollis-labs/tether/internal/shimbridge"
	"github.com/spf13/cobra"
)

// This command never loads a catalog or database. The daemon (future wiring)
// places the host first and gives the bridge an explicit private descriptor.
func shimBridgeCmd() *cobra.Command {
	var opts shimbridge.Options
	cmd := &cobra.Command{Use: "shim-bridge", Short: "Connect stdio to an explicitly placed shim", SilenceUsage: true, RunE: func(cmd *cobra.Command, _ []string) error {
		if opts.DescriptorPath == "" {
			return fmt.Errorf("--descriptor is required")
		}
		opts.OnAttach = func(event shimbridge.AttachEvent) error { return json.NewEncoder(cmd.ErrOrStderr()).Encode(event) }
		input, ok := cmd.InOrStdin().(interface {
			Read([]byte) (int, error)
			Close() error
		})
		if !ok {
			return fmt.Errorf("bridge stdin must be closable")
		}
		code, err := shimbridge.Run(cmd.Context(), opts, input, cmd.OutOrStdout(), cmd.ErrOrStderr())
		if err != nil {
			var fault *shimbridge.Failure
			if errors.As(err, &fault) {
				return fault
			}
			return err
		}
		if code != 0 {
			return &shimBridgeExit{code: code}
		}
		return nil
	}}
	cmd.Flags().StringVar(&opts.DescriptorPath, "descriptor", "", "private launch descriptor")
	cmd.Flags().StringVar(&opts.StatePath, "checkpoint", "", "private checkpoint (default beside descriptor)")
	cmd.Flags().BoolVar(&opts.Attach, "attach", false, "reattach without starting a child")
	cmd.Flags().StringVar(&opts.ExpectedJournal, "journal", "", "expected journal id (or use checkpoint)")
	cmd.Flags().StringVar(&opts.AfterCursor, "cursor", "", "expected locally committed source cursor")
	cmd.Flags().BoolVar(&opts.Takeover, "takeover", false, "explicitly replace the current controller")
	return cmd
}

type shimBridgeExit struct{ code int }

func (e *shimBridgeExit) Error() string {
	return fmt.Sprintf("hosted provider exited with status %d", e.code)
}
func init() { rootCmd.AddCommand(shimBridgeCmd()) }

func (e *shimBridgeExit) ExitCode() int { return e.code }
