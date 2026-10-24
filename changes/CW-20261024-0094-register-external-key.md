- Make registry registration retry-safe over HTTP (CW-20261024-0094). `POST
  /registry/{agents|projects}` accepts `substrate` and `external_id` (or one
  `external_key`, `<substrate>/<external_id>`): the first call creates the entry and
  answers `201`, a repeat with the same key answers `200` with the same URN and changes
  nothing, and concurrent calls agree on one URN. Half a key or a conflicting pair is
  `400`, never a silent plain register. `tether_registry_register` takes the same two
  optional arguments and reports `created`; the Go client gains
  `RegistryClient.RegisterIdempotent`. Without a key nothing changes. Nanite uses this
  with substrate `nanite` and external id `<install_id>/<host_settings_id>`.
