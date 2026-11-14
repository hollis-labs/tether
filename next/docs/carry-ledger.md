# Carry ledger

This records every `main` commit that reaches `substrate-port` through a `chore: merge main @<sha>` PR, and how the port carries it (plan section 2). Behaviour changes are carried as behaviour: code plus the test that proves it, never copied files.

## Rules

A slice is not done while a row in its area is `open`. The cutover gate is zero `open` rows up to the `main` SHA being replaced.

## Base

`substrate-port` was created from `main` at `f6c37bfa`. Everything up to that commit is the port's starting point and needs no row.

## Format

One row per `main` commit (squash-merged PR), oldest first, appended by the merge PR that brings it in:

| Column | Content |
|---|---|
| sha | short sha on `main` |
| PR | `#NNN` |
| task | Torque task id |
| area | a slice area: `issuer`, `http`, `registry`, `messaging`, `groups`, `events`, `sessions`, `shims`, `worker`, `teams`, `mcp`, `schema`, `catalog`, `cli`, `ops` |
| status | `ported`, `n/a` or `open` |
| carried by | for `ported`: the slice and the commit on `substrate-port`. For `n/a`: why (LLM gateway, sysop, old auth plumbing, per-version migration). |

Schema changes (`internal/store/migrations` `0102+`) also name the `migrate-from` transform in S12.

## Rows

| sha | PR | task | area | status | carried by |
|---|---|---|---|---|---|
