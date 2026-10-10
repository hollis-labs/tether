package main

import (
	"encoding/json"
	"io"

	"github.com/hollis-labs/tether/internal/client"
	directory "github.com/hollis-labs/tether/internal/environmentdirectory"
	"github.com/spf13/cobra"
)

var environmentDirectoryClientFactory = func() (*client.Client, error) { return newDaemonClient(catalogPath) }

// addEnvironmentDirectoryCommands attaches to the existing env command after
// the enrollment author's registration; every operation routes through daemon.
func addEnvironmentDirectoryCommands(command *cobra.Command) {
	list := &cobra.Command{Use: "list", Short: "List registered environments", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := environmentDirectoryClientFactory()
		if err != nil {
			return err
		}
		out, err := c.ListEnvironments(cmd.Context())
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}}
	get := &cobra.Command{Use: "get UUID", Short: "Get a registered environment", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := environmentDirectoryClientFactory()
		if err != nil {
			return err
		}
		out, err := c.GetEnvironment(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}}
	register := &cobra.Command{Use: "register", Short: "Register expected identity and durable routes from stdin JSON (local operator only)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dec := json.NewDecoder(io.LimitReader(cmd.InOrStdin(), 256<<10))
		dec.DisallowUnknownFields()
		var in directory.Registration
		if err := dec.Decode(&in); err != nil {
			return validationErr("invalid environment registration JSON")
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return validationErr("expected one registration object")
		}
		c, err := environmentDirectoryClientFactory()
		if err != nil {
			return err
		}
		out, err := c.RegisterEnvironment(cmd.Context(), in)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}}
	rename := &cobra.Command{Use: "rename UUID LABEL", Short: "Rename an environment's display label (local operator only)", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := environmentDirectoryClientFactory()
		if err != nil {
			return err
		}
		out, err := c.RenameEnvironment(cmd.Context(), args[0], args[1])
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}}
	remove := &cobra.Command{Use: "remove UUID", Short: "Retire an environment; unresolved device revocation remains pending (local operator only)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := environmentDirectoryClientFactory()
		if err != nil {
			return err
		}
		out, err := c.RetireEnvironment(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}}
	command.AddCommand(list, get, register, rename, remove)
}
