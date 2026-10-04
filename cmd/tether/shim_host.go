//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/spf13/cobra"
)

// shim-host owns only its explicit private descriptor and hosted process.
func shimHostCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{Use: "shim-host", Hidden: true, SilenceUsage: true, RunE: func(cmd *cobra.Command, _ []string) error {
		if path == "" {
			return fmt.Errorf("--launch is required")
		}
		spec, err := shim.ReadLaunch(path)
		if err != nil {
			return fmt.Errorf("shim descriptor refused")
		}
		// Install handlers before starting the child so teardown has no signal gap.
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		host, err := shim.Start(spec)
		if err != nil {
			return fmt.Errorf("shim host start refused")
		}
		<-ctx.Done()
		return host.Close()
	}}
	cmd.Flags().StringVar(&path, "launch", "", "private resolved launch descriptor")
	return cmd
}

func init() { rootCmd.AddCommand(shimHostCmd()) }
