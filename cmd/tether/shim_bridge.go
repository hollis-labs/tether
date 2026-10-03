//go:build !windows

package main

import (
	"encoding/json"
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
			return &shimBridgeFailure{cause: fmt.Errorf("--descriptor is required")}
		}
		opts.OnAttach = func(event shimbridge.AttachEvent) error { return json.NewEncoder(cmd.ErrOrStderr()).Encode(event) }
		input, ok := cmd.InOrStdin().(interface {
			Read([]byte) (int, error)
			Close() error
		})
		if !ok {
			return &shimBridgeFailure{cause: fmt.Errorf("bridge stdin must be closable")}
		}
		code, err := shimbridge.Run(cmd.Context(), opts, input, cmd.OutOrStdout(), cmd.ErrOrStderr())
		if err != nil {
			return &shimBridgeFailure{cause: err}
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

// InfrastructureFailureExit distinguishes bridge failure from ordinary child exit.
const InfrastructureFailureExit = 93

type shimBridgeFailure struct{ cause error }

func (e *shimBridgeFailure) Error() string { return e.cause.Error() }
func (e *shimBridgeFailure) Unwrap() error { return e.cause }
func (e *shimBridgeFailure) ExitCode() int { return InfrastructureFailureExit }
