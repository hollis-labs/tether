- Allow remote app service JWTs to fetch their own messages by ID and read
  registry agent profiles (CW-20261121-0142): `GET /messages/{id}?as=<bound>`
  requires `tether.read` and literal bound-address membership on `from` or `to`
  (returning 403 for other mailboxes and ignoring mutable aliases);
  `GET /registry/agents/{urn}` requires `tether.registry_read` (satisfied by
  `tether.read`) and remains strictly read-only.
