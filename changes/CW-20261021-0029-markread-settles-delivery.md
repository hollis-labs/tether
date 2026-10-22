- Marking a message read now settles its delivery (CW-20261021-0029, step 1): the
  delivery records the host_accepted, turn_submitted and consumed receipts, as Consume
  does, so a read message traces as consumed/delivered and a wake or lease lapse after
  the read has nothing left to retry. Idempotent, replay heals a crash between the two
  writes, only the addressed recipient can settle, and another claimant's lease is
  never taken over. `read_at` stays the core write; `consumed_at` is not touched. A
  delivery still inside its retry backoff cannot be claimed, so it is settled by the
  next wake attempt's handled check instead. No schema change.
