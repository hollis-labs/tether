- Events outbox ordering fix (CW-20261016-0136, X1a review of #398): `Bus.Publish` now drains committed
  outbox rows again after its own insert and before fan-out (a publisher that waited on the connection while a
  transaction committed a pending row would otherwise deliver its higher `Seq` first, and a cursor
  subscriber would drop the lower one for good), the drain clears its dirty flag before it looks, a bus over
  an outbox starts dirty so a crashed process's leftovers are drained by the first publish, a failed
  pre-drain stops `Publish` before it inserts, every `events` read path now returns `event_id`, and a repeated
  `PublishTx` returns the stored row.
