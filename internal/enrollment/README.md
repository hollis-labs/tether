# Enrollment custody primitives

CW-20261015-0052. This package implements cryptographic primitives only. It has
no HTTP routes, credential minting, approval policy, persistence or activation.
Do not expose it as an enrollment service without the durable controller and
reviewed shared operation contract.

Destination public keys are canonical unpadded base64url: Ed25519 for possession
and X25519 for delivery. Their SHA-256 RFC 7638 OKP JWK thumbprints use precisely
`crv`, `kty`, `x` in that order. Both thumbprints must be bound to the human-approved
immutable operation and server-owned versioned destination record. The current
shared operation v1 does not carry these fields; its closed parsers must not be
silently extended.

`Seal` uses HPKE base mode with DHKEM(X25519, HKDF-SHA256), HKDF-SHA256 and
AES-256-GCM. The ciphertext is the encapsulated key followed by ciphertext and
tag, encoded as canonical unpadded base64url. HPKE base mode does not authenticate
the sender: the helper must use an authenticated issuer connection and trusted
locally retained binding. Plaintext is limited to 4096 bytes; delivery expires
within ten minutes, with no positive clock leeway.

The HPKE info transcript begins with ASCII `tether-enrollment-delivery-v1` and a
NUL byte. It then contains these UTF-8 fields, each preceded by a four-byte
unsigned big-endian byte length:

1. Grant ID, operation digest, principal ID, destination ID, destination revision.
2. Signing thumbprint, encryption thumbprint.
3. Credential record ID, issuance timestamp, expiry timestamp.

Revisions are canonical positive unsigned decimal integers; timestamps and
credential IDs are canonical decimal integers in Unix seconds and storage ID
units respectively. Operation digests are `sha256:` plus 64 lowercase hex digits.
IDs are nonempty valid UTF-8 of at most 256 bytes without control characters.

Possession uses the same length framing with domain
`tether-enrollment-possession-v1`, the same seven binding fields, then action,
credential ID, nonce, issuance timestamp and expiry timestamp. A nonce is 32
cryptographically random bytes, canonical unpadded base64url. Challenges live at
most 60 seconds. Action `redeem` requires credential ID zero; action `confirm`
requires a positive credential ID. Ed25519 signs the entire transcript. These
transcripts are separate from JCS operation digests and WebAuthn challenges.

`Verify` proves possession against a **server-retained** challenge. It does not
consume that challenge or prevent replay on its own. The controller must consume
the nonce in the transaction that advances the approved grant. First redemption
must atomically retain a single credential hash and its sealed envelope. A retry
requires a fresh challenge and returns that same envelope; it must never mint a
second credential or seal a replacement. A confirmation proof deletes the
envelope and marks custody active. Expired unconfirmed custody must revoke the
orphan credential, delete ciphertext and require fresh approval.

The destination helper must verify the expected credential ID and grant state,
write the credential atomically with mode 0600 and checked owner, fsync before
confirmation, and keep its private signing/encryption keys local. Only public
keys, thumbprints, status and references belong in approval records or logs.
None of those controller/helper responsibilities are implemented by this slice.
