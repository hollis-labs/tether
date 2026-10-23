- Add `TETHER_SQLITE_SYNCHRONOUS` (`FULL`, `NORMAL`, `EXTRA`) to set the state database's synchronous
  pragma (CW-20261023-0018). Unset keeps SQLite's default, FULL, so nothing changes unless an
  operator sets it; an invalid value, `OFF` included, refuses to open the store. Shim retirement
  still requires FULL or EXTRA. Documented in `docs/messaging.md` with the durability trade-off.
