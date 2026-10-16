- Events outbox, part 2 (CW-20261016-0136, X1a): the store writes that bypassed the bus now go through
  the outbox: the reaper's `session.reaper` outcome and `session.activity`, the channel-attachment
  audit (`session.turn_routed`) and the team-replacement `session.replaced_by` lineage are written as
  pending rows on their own transaction (committed or rolled back with it) and published by the bus
  drainer after the commit, so live subscribers see them. The daemon starts the drainer
  (`StartEventsOutbox`), which also publishes rows a crashed process committed but never published.
  Only the daemon runs the drainer: a row written by another process (a CLI or a stdio `tether mcp`)
  stays pending until the daemon's next sweep publishes it, so it reaches live subscribers late, never lost.
