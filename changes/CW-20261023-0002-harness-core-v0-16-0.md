- Bump `harness/core` to v0.16.0 (CW-20261023-0002): the hub's event catalog now carries the `card.*`
  kinds (`card.created`, `updated`, `resolved`, `expired`, `effect.completed`, `effect.failed`) and the
  `tachyon` producer entry, so `POST /events` accepts them from producer app `tachyon` and refuses them
  from other apps. Their payload schemas are allowlists: a field the schema does not declare is not stored.
