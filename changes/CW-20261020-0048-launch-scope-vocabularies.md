- Fix worker launches from a paired device failing with `500 internal_error`
  (CW-20261020-0048): the launch ceiling compared a device's bare scopes
  (`operate`, `maintain`) to the profile's legacy names (`message.write`, ...)
  with an exact string match, so the default general-worker scopes shrank to none
  and the hub granted no session agent token. A worker launch now keeps exactly
  the profile scopes the launcher already holds, exactly or by containment
  (`identity.ScopeGranted`); it never adds or widens. The same comparison now
  applies to origin-session ceilings and replacement/native-only floors. A launch
  with no usable scope left is refused as `409 launch_scope_unavailable`, naming
  only the profile's scopes. The hub-local device launch is unchanged (a device
  child still gets no legacy scopes). See `docs/device-pairing.md`.
