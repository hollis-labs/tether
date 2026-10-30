- Service tokens can expire and can be limited to one substrate (CW-20261025-0001, CW-20261024-0023).
  `tether service-token issue --ttl 720h` sets a lifetime (minimum 5 minutes, default still ten years); key rotation
  re-issues with the original lifetime. `tether.registry:<substrate>` limits registry writes to actors that carry an
  external id of that substrate (register must be keyed under it; update and deregister look the entry up), and
  `tether.send_as:<substrate>` lets a credential author messages as such actors, which is what Nanite's bridge needs once
  sender identity enforces. Qualified scopes never satisfy the plain scope. Observe mode records violations, enforce
  mode refuses them. The `service-token` CLI now prints the daemon's reason for a refusal (it printed only "HTTP 400").
