- Slow migrations no longer look like a dead daemon (CW-20261115-0046).
  - **Progress file:** while the daemon applies migrations at start it keeps `<state.db>.migrating` (version, name,
    start time, pid, remaining versions) and logs each migration's elapsed time.
  - **`tether migrations status [--json]`** reports, read-only, the migrations a binary would apply to a state database
    and the one in progress.
  - **`tether migrations apply`** runs them as a separate step, before a binary swap.
  - **Docs:** CONTRIBUTING.md asks a PR whose migration takes over about 10 s on a production copy to say so.
