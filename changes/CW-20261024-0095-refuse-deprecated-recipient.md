- Refuse sends to a retired actor (CW-20261024-0095). A send or notify to an agent URN
  whose registry entry is deprecated now fails with `409 recipient_deprecated` and
  stores nothing, instead of queueing mail nobody will read. The tool error carries a
  typed pointer to `tether_registry_search` / `tether_registry_lookup_by`. Addresses the
  registry does not hold are unaffected, mail queued before the deprecation stays
  readable, and a registry lookup failure never blocks a send.
