package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// tether messages alias add|list|remove (CW-20261114-0067): address aliases.
// An alias is another name for a canonical address's mailbox; docs/messaging.md
// has the rules.

var messageAliasCanonical string

var messageAliasCmd = &cobra.Command{
	Use:   "alias",
	Short: "Manage address aliases (another name for an address's mailbox)",
}

var messageAliasAddCmd = &cobra.Command{
	Use:   "add <alias-urn> <canonical-urn>",
	Short: "Declare alias-urn another name for canonical-urn (operator only)",
	Long: "Mail sent to the alias is stored under the canonical address, and reading either\n" +
		"address returns the mail of both. Mail already stored under the alias is not\n" +
		"rewritten; it becomes visible in the canonical mailbox with its read and archive\n" +
		"state unchanged.",
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AddressAliasAdd(cmdCtx(cmd), args[0], args[1])
		if err != nil {
			return classifyErr(err)
		}
		if messageJSONFlag {
			fmt.Println(string(out))
			return nil
		}
		var row struct {
			Alias          string `json:"alias"`
			Canonical      string `json:"canonical"`
			MergedMessages int    `json:"merged_messages"`
		}
		if err := json.Unmarshal(out, &row); err != nil {
			return classifyErr(err)
		}
		fmt.Printf("alias: %s -> %s (%d stored message(s) under the alias now also listed under the canonical address)\n", row.Alias, row.Canonical, row.MergedMessages)
		return nil
	},
}

var messageAliasListCmd = &cobra.Command{
	Use:   "list",
	Short: "List address aliases",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AddressAliasList(cmdCtx(cmd), messageAliasCanonical)
		if err != nil {
			return classifyErr(err)
		}
		if messageJSONFlag {
			fmt.Println(string(out))
			return nil
		}
		var body struct {
			Aliases []struct {
				Alias     string `json:"alias"`
				Canonical string `json:"canonical"`
			} `json:"aliases"`
		}
		if err := json.Unmarshal(out, &body); err != nil {
			return classifyErr(err)
		}
		if len(body.Aliases) == 0 {
			fmt.Println("no address aliases")
			return nil
		}
		for _, a := range body.Aliases {
			fmt.Printf("%s -> %s\n", a.Alias, a.Canonical)
		}
		return nil
	},
}

var messageAliasRemoveCmd = &cobra.Command{
	Use:   "remove <alias-urn>",
	Short: "Remove an address alias (operator only)",
	Long: "Mail sent to the alias while it was one stays under the canonical address; mail\n" +
		"stored under the alias itself is again listed only under the alias.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.AddressAliasRemove(cmdCtx(cmd), args[0])
		if err != nil {
			return classifyErr(err)
		}
		if messageJSONFlag {
			fmt.Println(string(out))
			return nil
		}
		fmt.Printf("removed alias: %s\n", args[0])
		return nil
	},
}

func init() {
	messageAliasListCmd.Flags().StringVar(&messageAliasCanonical, "canonical", "", "only aliases of this address")
	messageAliasCmd.AddCommand(messageAliasAddCmd, messageAliasListCmd, messageAliasRemoveCmd)
	messagesCmd.AddCommand(messageAliasCmd)
}
