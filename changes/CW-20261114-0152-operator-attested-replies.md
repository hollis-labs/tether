- Operator-attested replies (CW-20261114-0152, docs/operator-attested-replies.md): Chrispian answers a typed card
  (question, decision, approval_request) from Tachyon, and Tether stores it as his verified reply (`from_principal` the
  operator, `from_verification` verified, `operator_attestation` metadata). The proof is a Tangent-signed Ed25519 JWS
  over a single-use challenge, at most 300 s old whatever `elevation_ttl` says.
  - New routes: `POST /messages/{id}/operator-reply/challenge`, `POST /messages/{id}/operator-reply` and
    `GET /operator-attestation/challenges/{id}` (and `/body`).
  - New device scopes `message.operator_reply` and `message.operator_reply_view`.
  - New privileged global.yaml block `operator_attestation`. It is applied only by an operator-approved plan, and a
    reload with any other block is refused and audited.
  - Migration 0106 adds the challenge, proof, answer and trust-root tables and an `identity_audit.detail` column.
  - Off until an approved block exists.
  - `operator_attestation` (any key under that prefix) and the sender stamp (`from_verification`, `from_unverified`,
    `from_principal`) are reserved: every ingress drops a client's value, and the store keeps `operator_attestation`
    only on the attested-reply path.
