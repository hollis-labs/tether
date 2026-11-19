- tether sysop reaches Cerberus at a configured address (CW-20261114-0145): `CERBERUS_ADDR` (`unix:/path` or an
  `http(s)://` base, e.g. the new Cerberus at `http://127.0.0.1:4786/api`) and `CERBERUS_TOKEN_FILE`
  (bearer token, re-read per call; plain http only to loopback or the tailnet, https anywhere, anything else refused at
  start). Unset, resource actions use `~/.cerberus/cerberus.sock` exactly as before. Both the
  old `{success, error}` and the new flat `{code, message, details}` error bodies are reported (its `message`). Rollback: unset both.
