package mcpadapter

import (
	"context"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
)

// Address alias tools (CW-20261114-0067). An alias is another name for a
// canonical address's mailbox. The daemon refuses add and remove from anyone
// but the operator credential.

func (a *Adapter) registerAddressAliasTools(s *gomcp.Server) {
	a.addTool(s, strictArgs(gomcp.Tool{
		Name: "tether_address_alias_list",
		Description: "List address aliases: each alias URN and the canonical address whose mailbox it names. " +
			"Mail sent to an alias is stored under its canonical address, and reading either returns the mail of both.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("canonical", "Only aliases of this address (optional)", false),
		),
		Handler: a.handleAddressAliasList,
	}), Reads("lists address aliases; changes nothing"))

	a.addTool(s, strictArgs(gomcp.Tool{
		Name: "tether_address_alias_add",
		Description: "Declare an address an alias of a canonical address (operator only; every other caller is refused). " +
			"Mail to the alias is then stored under the canonical address. Mail already stored under the alias is not rewritten: " +
			"it becomes visible in the canonical mailbox, read and archive state unchanged. Aliases are one hop: the canonical address must not itself be an alias. " +
			"Requires registry.write scope.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("alias", "The old or alternative address (msg://agent/... or msg://user/...)", true),
			gomcp.StringProp("canonical", "The address mail is stored under (msg://agent/... or msg://user/...)", true),
		),
		Handler: a.handleAddressAliasAdd,
	}), Writes())

	a.addTool(s, strictArgs(gomcp.Tool{
		Name: "tether_address_alias_remove",
		Description: "Remove an address alias (operator only). Mail sent to the alias while it was one stays under the canonical address; " +
			"mail stored under the alias itself is again listed only under the alias. Requires registry.write scope.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("alias", "The alias address to remove", true),
		),
		Handler: a.handleAddressAliasRemove,
	}), Writes())
}

func (a *Adapter) handleAddressAliasList(ctx context.Context, args map[string]any) (any, error) {
	if err := a.daemonClientFor("tether_address_alias_list"); err != nil {
		return nil, err
	}
	out, err := a.client.AddressAliasList(ctx, str(args, "canonical"))
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(out), nil
}

func (a *Adapter) handleAddressAliasAdd(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeRegistryWrite); err != nil {
		return nil, err
	}
	if err := a.daemonClientFor("tether_address_alias_add"); err != nil {
		return nil, err
	}
	alias, canonical := str(args, "alias"), str(args, "canonical")
	if alias == "" || canonical == "" {
		return nil, toolError("invalid_request", "alias and canonical are required")
	}
	out, err := a.client.AddressAliasAdd(ctx, alias, canonical)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(out), nil
}

func (a *Adapter) handleAddressAliasRemove(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeRegistryWrite); err != nil {
		return nil, err
	}
	if err := a.daemonClientFor("tether_address_alias_remove"); err != nil {
		return nil, err
	}
	alias := str(args, "alias")
	if alias == "" {
		return nil, toolError("invalid_request", "alias is required")
	}
	out, err := a.client.AddressAliasRemove(ctx, alias)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, classifyClientErr(err, "")
	}
	return toolJSON(out), nil
}
