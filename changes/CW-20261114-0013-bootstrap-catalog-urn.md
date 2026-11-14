- Registry bootstrap honours the catalog `urn:` (CW-20261114-0013): an agent that names its registry identity is that
  row; bootstrap attaches its `tether` id (and the handle backfill its `@handle`) there instead of minting a duplicate,
  and an `urn:` that is not registered is reported, not created. Before this, a bootstrap run minted a second row for
  every agent registered outside the catalog and moved its ids onto it.
- `tether registry merge --dry-run` lists the external ids that will move and those that stay (dst already has the
  substrate) (CW-20261114-0013); it used to read profiles without external ids and always printed 0.
