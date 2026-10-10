package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/client"
)

var remoteTarget string

func init() {
	rootCmd.PersistentFlags().StringVar(&remoteTarget, "target", "", "remote worker via SSH forward (tcp:localhost:port); requires a device credential")
	rootCmd.PersistentPreRunE = guardRemoteCommand
}

func remoteTargetSelected() bool {
	return remoteTarget != "" || rootCmd.PersistentFlags().Changed("target")
}

func validateRemoteTarget(target string) error {
	if !strings.HasPrefix(target, "tcp:") {
		return fmt.Errorf("--target must be tcp:localhost:port for an SSH-forwarded worker")
	}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(target, "tcp:"))
	ip := net.ParseIP(host)
	if err != nil || host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("--target must use a loopback host and explicit port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("--target port must be between 1 and 65535")
	}
	return nil
}

// A closed command admission list refuses host-only handlers before they can
// discover a catalog, open a DB/file, invoke an editor, or manage a service.
// It also makes newly added commands local-only until audited for API parity.
func guardRemoteCommand(cmd *cobra.Command, _ []string) error {
	if !remoteTargetSelected() {
		return nil
	}
	if err := validateRemoteTarget(remoteTarget); err != nil {
		return err
	}
	parts := strings.Fields(cmd.CommandPath())
	if len(parts) < 2 {
		return fmt.Errorf("%s is unavailable with --target", cmd.CommandPath())
	}
	switch strings.Join(parts[1:], " ") {
	case "sessions list", "sessions get", "sessions stop", "sessions tail", "sessions attach", "sessions input", "sessions turn":
		return nil
	case "launch":
		for _, flag := range []string{"agent-file", "agent-inline", "boot-profile", "override", "injection", "torque-task"} {
			if cmd.Flags().Changed(flag) {
				return fmt.Errorf("--%s is unavailable with --target; host launch inputs require an explicit remote API contract", flag)
			}
		}
		return nil
	default:
		return fmt.Errorf("%s requires local host access and is unavailable with --target", cmd.CommandPath())
	}
}

func remoteDaemonClient() (*client.Client, error) {
	if err := validateRemoteTarget(remoteTarget); err != nil {
		return nil, err
	}
	if tokenFilePath != "" {
		return client.New(remoteTarget, client.WithTokenFile(tokenFilePath)), nil
	}
	if token := os.Getenv("TETHER_TOKEN"); token != "" {
		return client.New(remoteTarget, client.WithToken(token)), nil
	}
	return nil, fmt.Errorf("--target requires an explicit device credential (--token-file or TETHER_TOKEN)")
}

func runRemoteTailSnapshot(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c, err := remoteDaemonClient()
	if err != nil {
		return err
	}
	res, err := c.SessionLog(ctx, id, client.SessionLogOptions{})
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(res.Data)
	return err
}
