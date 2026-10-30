- Add an `identity.points.sender` enforcement point (CW-20261024-0023). Forged `from` and `as` claims can now be enforced on
  their own (`identity.points.sender: enforce`) while `local_api` stays at observe, so enforcing sender identity does not also
  enforce route scopes (which would refuse about 1,200 read calls from session credentials in the last week). It follows the global
  mode when unset, so nothing changes until it is set. See docs/identity-enforce-runbook.md.
