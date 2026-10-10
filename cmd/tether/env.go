package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/sshenroll"
	"github.com/spf13/cobra"
)

type enrollmentManagerFactory func(string) *sshenroll.Manager

func defaultEnrollmentManager(target string) *sshenroll.Manager {
	return &sshenroll.Manager{Remote: &sshenroll.SSH{Target: target, StepTimeout: 30 * time.Second}}
}

func newEnvironmentCommand(factory enrollmentManagerFactory) *cobra.Command {
	command := &cobra.Command{Use: "env", Short: "Explicit SSH worker enrollment and its retained receipt"}
	var o sshenroll.Options
	add := &cobra.Command{Use: "add <ssh-target>", Short: "Enroll a dedicated Linux worker from a verified local release", Args: cobra.ExactArgs(1), SilenceUsage: true, RunE: func(cmd *cobra.Command, args []string) error {
		o.Target = args[0]
		if o.ReceiptDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("cannot locate private enrollment storage")
			}
			o.ReceiptDir = filepath.Join(home, ".tether", "environments", o.Authority)
		}
		r, err := factory(o.Target).Add(cmd.Context(), o)
		if err != nil {
			if r.OperationID != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Enrollment partial: operation %s, phase %s. Private receipt retained.\n", r.OperationID, r.Phase)
			}
			return err
		}
		return printEnrollmentReceipt(cmd.OutOrStdout(), r)
	}}
	add.Flags().StringVar(&o.Authority, "authority", "", "worker environment authority name")
	add.Flags().StringVar(&o.Version, "version", "", "exact release version, no tags or ranges")
	add.Flags().StringVar(&o.Archive, "archive", "", "local release archive for the worker architecture")
	add.Flags().StringVar(&o.Checksums, "checksums", "", "local release checksums.txt")
	add.Flags().StringVar(&o.ReceiptDir, "receipt-dir", "", "absolute private hub receipt directory")
	add.Flags().StringSliceVar(&o.Providers, "provider", nil, "preinstalled provider CLI names to require on the login PATH")
	add.Flags().StringSliceVar(&o.Scopes, "scope", []string{identity.ScopeRead}, "explicit independent device scopes; read is required for verification")
	add.Flags().IntVar(&o.RemotePort, "remote-port", 7181, "worker loopback listener port")
	add.Flags().DurationVar(&o.Timeout, "timeout", 10*time.Minute, "overall enrollment deadline (maximum 30m)")
	for _, name := range []string{"authority", "version", "archive", "checksums", "provider"} {
		_ = add.MarkFlagRequired(name)
	}
	command.AddCommand(add)
	var receiptDir string
	rollback := &cobra.Command{Use: "rollback <ssh-target>", Short: "Remove only the exact managed worker unit; retain runtimes and state", Args: cobra.ExactArgs(1), SilenceUsage: true, RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		r, err := factory(args[0]).Rollback(ctx, receiptDir)
		if err != nil {
			return err
		}
		return printEnrollmentReceipt(cmd.OutOrStdout(), r)
	}}
	rollback.Flags().StringVar(&receiptDir, "receipt-dir", "", "absolute private receipt directory for this exact enrollment")
	_ = rollback.MarkFlagRequired("receipt-dir")
	command.AddCommand(rollback)
	command.AddCommand(newWorkerEnrollmentCommand())
	return command
}

func printEnrollmentReceipt(out io.Writer, r sshenroll.Receipt) error {
	// The operation receipt intentionally has no device token or grant code.
	// It also has no ephemeral forward URL or inferred agent-management mode.
	return json.NewEncoder(out).Encode(struct {
		OperationID, EnvironmentID, Authority, Version, DeviceID, CredentialReference, Phase string
		RemotePort                                                                           int
		DirectoryRegistration                                                                string
	}{r.OperationID, r.EnvironmentID, r.Authority, r.Version, r.DeviceID, r.CredentialReference, r.Phase, r.RemotePort, "pending explicit durable routes and directory registration"})
}

func workerOperatorClient(home string) (*deviceAdminClient, error) {
	cfg, err := loadDaemonConfig(filepath.Join(home, ".tether", "catalog"))
	if err != nil {
		return nil, fmt.Errorf("worker local catalog unavailable")
	}
	if cfg.ListenAddr != "unix:"+filepath.Join(home, ".tether", "run", "tetherd.sock") {
		return nil, fmt.Errorf("worker pairing requires its exact local Unix socket")
	}
	token, err := identity.ReadTokenFile(filepath.Join(home, ".tether", "run", "operator.token"))
	if err != nil {
		return nil, fmt.Errorf("worker local operator credential unavailable")
	}
	hc := daemon.DialHTTPClient(cfg.ListenAddr)
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &deviceAdminClient{client: hc, baseURL: daemon.BaseURL(cfg.ListenAddr), token: token}, nil
}

// The private SSH helper consumes input on stdin. Grant output is a private
// captured pipe, never a command argument, journal entry or public receipt.
func newWorkerEnrollmentCommand() *cobra.Command {
	return &cobra.Command{Use: "__worker <install|inspect|grant|rollback>", Hidden: true, Args: cobra.ExactArgs(1), SilenceUsage: true, SilenceErrors: true, RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "install", "inspect", "grant", "rollback":
		default:
			return fmt.Errorf("unknown worker enrollment action")
		}
		data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), (64<<10)+1))
		var request sshenroll.WorkerRequest
		if err != nil || len(data) > 64<<10 || json.Unmarshal(data, &request) != nil {
			return fmt.Errorf("invalid private worker request")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("worker home unavailable")
		}
		w, err := sshenroll.OpenWorker(cmd.Context(), home, request)
		if err != nil {
			return err
		}
		defer w.Close()
		catalogPath = filepath.Join(home, ".tether", "catalog")
		if args[0] == "install" {
			if err := w.Prepare(func() error {
				return runInit(bytes.NewReader(nil), io.Discard, initOpts{StateDir: filepath.Join(home, ".tether"), Yes: true})
			}); err != nil {
				return err
			}
		}
		manager, err := workerServiceFactory()
		if err != nil {
			return fmt.Errorf("worker service configuration unavailable")
		}
		var output any
		switch args[0] {
		case "install":
			output, err = w.Install(cmd.Context(), manager)
		case "inspect":
			output, err = w.Inspect(cmd.Context(), manager)
		case "grant":
			if _, err = w.Inspect(cmd.Context(), manager); err != nil {
				return err
			}
			client, e := workerOperatorClient(home)
			if e != nil {
				return e
			}
			var grant identity.IssuedGrant
			err = client.call(cmd.Context(), http.MethodPost, "/auth/pair", api.PairRequest{Label: "enrollment-" + request.OperationID, Scopes: request.Scopes, TTL: identity.DefaultGrantTTL.String()}, &grant)
			output = grant
		case "rollback":
			output, err = w.Rollback(cmd.Context(), manager, func(id string) error {
				client, e := workerOperatorClient(home)
				if e != nil {
					return e
				}
				var listed struct {
					Devices []identity.Device `json:"devices"`
				}
				if err := client.call(cmd.Context(), http.MethodGet, "/auth/devices", nil, &listed); err != nil {
					return err
				}
				matched := false
				for _, device := range listed.Devices {
					if device.ID == id && device.Label == "enrollment-"+request.OperationID {
						matched = true
					}
				}
				if !matched {
					return fmt.Errorf("paired device does not match this enrollment operation")
				}
				return client.call(cmd.Context(), http.MethodPost, "/auth/revoke", api.DeviceRevokeRequest{ID: id}, nil)
			}, request.DeviceID)
		default:
			return fmt.Errorf("unknown worker enrollment action")
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(output)
	}}
}
