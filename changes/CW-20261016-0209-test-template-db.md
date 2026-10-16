- Faster tests (CW-20261016-0209): 17 more test packages (mcpadapter, teamhost, teamruntime, mcptransport, fabricbinding,
  fabricenrollment, a2aadapter, definitionresolve, identity, teamstore, turnrouting, fabricstore, forums, federation, channels,
  sshenroll, environmentstream) start their SQLite stores from the one migrated template image `store.UseMigratedTemplate` already
  gave six packages, instead of replaying every migration (now 46 ALTERs longer since 0082) per `store.Open`. Test-only: no
  production code or migration changed, and an existing database file is opened exactly as before.
