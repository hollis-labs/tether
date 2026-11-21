package main

import (
	"encoding/json"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/spf13/cobra"
)

func newCorrelationCommand() *cobra.Command {
	root := &cobra.Command{Use: "correlation", Short: "Register external kit-session correlation facts"}
	var in api.CorrelationRegisterRequest
	register := &cobra.Command{Use: "register", Short: "Queue best-effort Registry registration (local operator credential)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := in.Validate(); err != nil {
			return validationErr("%s", err.Error())
		}
		c, err := registryClientFactory()
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.RegisterCorrelation(cmdCtx(cmd), in)
		if err != nil {
			return classifyErr(err)
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}}
	f := register.Flags()
	f.StringVar(&in.URN, "urn", "", "agent msg:// URN (required)")
	f.StringVar(&in.Session, "session", "", "external provider session identifier (required)")
	f.StringVar(&in.RunID, "run-id", "", "launcher execution run identifier (defaults to session)")
	f.StringVar(&in.TaskID, "task", "", "Torque task identifier")
	f.StringVar(&in.WorkstreamID, "workstream", "", "canonical Registry workstream:ULID")
	f.StringVar(&in.ManifestDigest, "manifest-digest", "", "committed boot manifest digest")
	f.StringVar(&in.ExportReceiptID, "export-receipt-id", "", "boot export receipt identifier")
	f.StringVar(&in.ActivitySHA256, "activity-sha256", "", "ACTIVITY.md SHA-256 (64 hex characters)")
	f.StringVar(&in.ProviderSessionID, "provider-session-id", "", "provider native session identifier")
	root.AddCommand(register)
	return root
}
