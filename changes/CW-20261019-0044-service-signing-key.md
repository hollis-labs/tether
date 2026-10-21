- Sign service tokens with a dedicated key that is not pruned (CW-20261019-0044):
  service tokens (ten years) now use their own `svc-` Ed25519 key, so the ordinary
  signing-key rotation and prune can no longer silently invalidate them. The key is
  kept while any live service token record names it and is deleted only after it was
  rotated out and none does. New: `tether service-token rotate-key [--reissue
  --out-dir DIR]` (old tokens keep working until revoked; replacements go to 0600
  files, never to the terminal) and `tether service-token revoke --all [--app A]`
  (emergency stop). The JWKS publishes the key as before. See
  `docs/service-tokens.md` for the blast radius.
