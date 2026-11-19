- Teams Stage B (CW-20261118-0046): an orchestrator-led team shape (agent
  orchestrator, one to three workers, one reviewer, at most five agents) with
  fixed authority (workers and the reviewer may message only the orchestrator),
  policy-listed service principals (`services:` in `teams-policy.yaml`) that may
  form and dissolve teams, generous per-policy ceilings (`ceilings:` default and
  max, replacing the five-minute conservative limits), per-slot launches with a
  reviewer-pairing preference mode, and automatic dissolve on the orchestrator's
  result, the timeout ceiling or when no member is live (new
  `team_run_deadlines` table, migration 0108). See `docs/teams-activation.md`.
