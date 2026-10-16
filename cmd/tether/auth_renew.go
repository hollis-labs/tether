package main

import (
	"context"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/devicepair"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/secretsetup"
	"github.com/spf13/cobra"
)

func newDeviceRenewCommand() *cobra.Command {
	var hub, file, ref, host string
	cmd := &cobra.Command{Use: "renew", Short: "Extend a paired device credential without changing its bearer or scopes", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if hub == "" || (file == "") == (ref == "") {
				return fmt.Errorf("provide --hub and exactly one of --credential-file or --credential-ref")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
			defer cancel()
			var token string
			var err error
			if file != "" {
				token, err = identity.ReadTokenFile(file)
			} else {
				r, e := secretsetup.New().Resolve(ctx, ref)
				err = e
				if err == nil {
					token = r.Secret.Reveal()
				}
			}
			if err != nil {
				return fmt.Errorf("device credential could not be resolved")
			}
			renewed, err := devicepair.RenewWithHost(ctx, nil, hub, token, host)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Device credential renewed until %s; bearer and scopes unchanged.\n", renewed.ExpiresAt.UTC().Format(time.RFC3339))
			return err
		}}
	cmd.Flags().StringVar(&host, "host", "", "allowed HTTP Host override for a loopback listener")
	cmd.Flags().StringVar(&hub, "hub", "", "enrolled hub HTTP(S) URL")
	cmd.Flags().StringVar(&file, "credential-file", "", "owned 0600 device token file")
	cmd.Flags().StringVar(&ref, "credential-ref", "", "go-secrets reference, e.g. file://tesseract/denylist-bearer")
	return cmd
}
