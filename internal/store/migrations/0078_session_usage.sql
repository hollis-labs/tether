-- Per-session usage meter (CW-20261014-0042): recorded from terminal turn
-- events. Token and cost columns only count turns whose runtime reported
-- usage; usage_turns says how many did, so "unreported" is distinguishable
-- from zero.
CREATE TABLE session_usage (
  session_id TEXT PRIMARY KEY,
  turns INTEGER NOT NULL DEFAULT 0,
  failed_turns INTEGER NOT NULL DEFAULT 0,
  usage_turns INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0,
  turn_ms INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);
