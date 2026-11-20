package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func init() {
	approvers := &cobra.Command{Use: "approvers", Short: "Inspect delegated catalog approvers"}
	for _, name := range []string{"list", "show"} {
		approvers.AddCommand(&cobra.Command{Use: name + " [principal]", Short: "Read approver policy and breakglass state", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			c, err := localOnlyClient(catalogPath)
			if err != nil {
				return err
			}
			v, err := c.CatalogApply().Approvers(cmd.Context())
			if err != nil {
				return err
			}
			if len(args) > 0 {
				p, ok := v.Policy.Principals[args[0]]
				if !ok {
					for family, legacy := range v.LegacyDefaults {
						base := strings.TrimSuffix(family, "[/<env>]")
						if args[0] == base || strings.HasPrefix(args[0], base+"/") {
							p = legacy
							ok = true
							break
						}
					}
				}
				if !ok {
					return errors.New("principal not found in explicit policy; inspect legacy_tachyon for built-in defaults")
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"principal": args[0], "policy": p, "breakglass": v.Breakglass})
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(v)
		}})
	}
	var principal, reason, duration string
	glass := &cobra.Command{Use: "breakglass", Short: "Open a bounded operator-approved delegation window", Args: cobra.NoArgs}
	run := func(end bool) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			if !end {
				d, err := time.ParseDuration(duration)
				if err != nil || d <= 0 || d > 4*time.Hour || principal == "" || reason == "" {
					return errors.New("provide --principal, --reason and --for (positive, at most 4h)")
				}
			}
			c, err := localOnlyClient(catalogPath)
			if err != nil {
				return err
			}
			tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
			if err != nil {
				return errors.New("breakglass needs an operator terminal")
			}
			defer tty.Close() //nolint:errcheck
			if !term.IsTerminal(int(tty.Fd())) {
				return errors.New("breakglass needs an operator terminal")
			}
			fmt.Fprint(tty, "Approval passphrase: ")
			pass, err := term.ReadPassword(int(tty.Fd()))
			fmt.Fprintln(tty)
			if err != nil {
				return err
			}
			defer func() {
				for i := range pass {
					pass[i] = 0
				}
			}()
			out, err := c.CatalogApply().Breakglass(cmd.Context(), api.CatalogBreakglassRequest{Principal: principal, Reason: reason, For: duration, Passphrase: string(pass), End: end})
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
		}
	}
	glass.RunE = run(false)
	glass.Flags().StringVar(&principal, "principal", "", "exact approver principal URN")
	glass.Flags().StringVar(&reason, "reason", "", "operator reason (no secrets)")
	glass.Flags().StringVar(&duration, "for", "", "window duration, at most 4h")
	glass.AddCommand(&cobra.Command{Use: "end", Short: "Close the active window (operator passphrase required)", Args: cobra.NoArgs, RunE: run(true)})
	catalogCmd.AddCommand(approvers, glass)
}
