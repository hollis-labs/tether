- Document which listener accepts a service token (CW-20261024-0094): the local listener, and the remote listener for
  `POST /events` only, so a 401 on any other route from a service means the wrong surface rather than a bad token. A
  service on the hub host reaches the registry and other routes over the daemon's unix socket with a Bearer token; the
  new section in `docs/auth-scopes.md` has the call shape, how to confirm the principal with `GET /auth/context`, and how
  revocation behaves. Found when Nanite's first registry call went to the public edge and got 401.
