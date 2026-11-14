- tether sysop reaches Cerberus at a configured address (CW-20261114-0145): `CERBERUS_ADDR` (`unix:/path` or an
  `http(s)://` base, e.g. the new Cerberus at `http://127.0.0.1:4786/api/cerberus/legacy`) and `CERBERUS_TOKEN_FILE`
  (bearer token, re-read per call). Unset, resource actions use `~/.cerberus/cerberus.sock` exactly as before. Both the
  old `{success, error}` and the new `{code, message}` error bodies are reported. Rollback: unset both.
