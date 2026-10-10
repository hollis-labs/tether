package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/service"
)

var workerServiceFactory = func() (*service.Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	account, err := user.Current()
	if err != nil {
		return nil, err
	}
	catalog, err := filepath.Abs(catalogPath)
	if err != nil {
		return nil, err
	}
	// This is configuration reading only: service management never opens the
	// application/store, seeds a catalog, enrolls an agent or mints credentials.
	cat, err := config.Load(catalog)
	if err != nil {
		return nil, &service.Problem{Code: "catalog-unavailable", Message: err.Error(), Hint: "Prepare the worker catalog with tether init first."}
	}
	cfg, err := daemonConfigFromCommand(cat, serveCmd)
	if err != nil {
		return nil, &service.Problem{Code: "catalog-unavailable", Message: err.Error(), Hint: "Prepare the worker catalog with tether init first."}
	}
	configRoot := os.Getenv("XDG_CONFIG_HOME")
	if configRoot == "" {
		configRoot = filepath.Join(home, ".config")
	}
	if !filepath.IsAbs(configRoot) {
		return nil, fmt.Errorf("XDG_CONFIG_HOME must be absolute")
	}
	return &service.Manager{
		Runtime: service.Runtime{Root: filepath.Join(home, ".tether", "runtime")},
		UnitDir: filepath.Join(configRoot, "systemd", "user"),
		Catalog: catalog, PathEnv: os.Getenv("PATH"), UID: account.Uid, User: account.Username,
		DaemonPID: func() (int, error) {
			pid, err := daemon.ReadPIDFile(cfg.PIDFile)
			if os.IsNotExist(err) {
				return 0, nil
			}
			if err != nil {
				return 0, err
			}
			verified, err := daemon.IsDaemon(context.Background(), pid)
			if err != nil {
				return 0, err
			}
			if verified {
				return pid, nil
			}
			return 0, nil
		},
	}, nil
}

func protocolServiceHint() string {
	return fmt.Sprintf("This client speaks protocol %d. Fetch %s; on protocol_mismatch, update the client or environment to the required_protocol. Shim wire negotiation remains unchanged.", environment.Protocol, environment.DescriptorPath)
}

func newWorkerServiceCommand() *cobra.Command {
	command := &cobra.Command{Use: "service", Short: "Manage an explicitly owned Linux worker user service"}
	command.PersistentFlags().Bool("json", false, "Print service status as JSON")
	printStatus := func(cmd *cobra.Command, m *service.Manager) service.Status {
		status := m.Status(cmd.Context())
		status.ProtocolHint = protocolServiceHint()
		jsonOutput, _ := cmd.Flags().GetBool("json")
		if jsonOutput {
			_ = json.NewEncoder(cmd.OutOrStdout()).Encode(status)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Tether worker service\n  ownership: %s\n  current: %s\n  previous: %s\n  active: %s\n  enabled: %s\n  linger: %s\n", status.Ownership, status.Current, status.Previous, status.Active, status.Enabled, status.Linger)
			for _, p := range status.Problems {
				fmt.Fprintf(cmd.OutOrStdout(), "  [%s] %s\n", p.Code, p.Message)
				if p.Hint != "" {
					fmt.Fprintln(cmd.OutOrStdout(), "  "+p.Hint)
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), status.ProtocolHint)
		}
		return status
	}
	for _, action := range []string{"install", "update"} {
		op := action
		child := &cobra.Command{
			Use: op + " <exact-version>", Short: op + " a checksummed local release archive", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := service.ValidateVersion(args[0]); err != nil {
					return err
				}
				archive, _ := cmd.Flags().GetString("archive")
				checksums, _ := cmd.Flags().GetString("checksums")
				m, err := workerServiceFactory()
				if err != nil {
					return err
				}
				if op == "install" {
					err = m.Install(cmd.Context(), args[0], archive, checksums)
				} else {
					err = m.Update(cmd.Context(), args[0], archive, checksums)
				}
				if err != nil {
					return err
				}
				status := printStatus(cmd, m)
				if len(status.Problems) > 0 {
					return &status.Problems[0]
				}
				return nil
			},
		}
		child.Flags().String("archive", "", "Local tether_<version>_linux_<arch>.tar.gz")
		child.Flags().String("checksums", "", "Local release checksums.txt")
		_ = child.MarkFlagRequired("archive")
		_ = child.MarkFlagRequired("checksums")
		command.AddCommand(child)
	}
	command.AddCommand(&cobra.Command{Use: "status", Short: "Explain runtime, ownership, service and linger state", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		m, err := workerServiceFactory()
		if err != nil {
			return err
		}
		status := printStatus(cmd, m)
		if len(status.Problems) > 0 {
			return &status.Problems[0]
		}
		return nil
	}})
	command.AddCommand(&cobra.Command{Use: "restart", Short: "Restart only the verified managed worker unit", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		m, err := workerServiceFactory()
		if err != nil {
			return err
		}
		if err := m.Restart(cmd.Context()); err != nil {
			return err
		}
		status := printStatus(cmd, m)
		if len(status.Problems) > 0 {
			return &status.Problems[0]
		}
		return nil
	}})
	command.AddCommand(&cobra.Command{Use: "uninstall", Short: "Stop and remove the managed unit; retain runtimes and user data", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		m, err := workerServiceFactory()
		if err != nil {
			return err
		}
		if err := m.Uninstall(cmd.Context()); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Managed worker unit removed. Runtime versions, catalog, state and credentials retained.")
		return nil
	}})
	command.AddCommand(&cobra.Command{Use: "switch-back [exact-version]", Short: "Restart on a retained version (default: previous)", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		version := ""
		if len(args) > 0 {
			version = args[0]
			if err := service.ValidateVersion(version); err != nil {
				return err
			}
		}
		m, err := workerServiceFactory()
		if err != nil {
			return err
		}
		if err := m.SwitchBack(cmd.Context(), version); err != nil {
			return err
		}
		status := printStatus(cmd, m)
		if len(status.Problems) > 0 {
			return &status.Problems[0]
		}
		return nil
	}})
	return command
}
