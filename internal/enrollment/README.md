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

The private `seal` primitive uses HPKE base mode with DHKEM(X25519, HKDF-SHA256), HKDF-SHA256 and
AES-256-GCM. The ciphertext is the encapsulated key followed by ciphertext and
tag, encoded as canonical unpadded base64url. HPKE base mode does not authenticate
the sender: the helper must use an authenticated issuer connection and trusted
locally retained binding. Plaintext is limited to 4096 bytes; delivery expires
within ten minutes, with no positive clock leeway.

The HPKE info transcript begins with ASCII `tether-enrollment-delivery-v2` and a
NUL byte. It then contains these UTF-8 fields, each preceded by a four-byte
unsigned big-endian byte length:

1. Configured issuer, grant ID, operation digest, principal ID, destination ID, destination revision.
2. Signing thumbprint, encryption thumbprint.
3. Credential record ID, issuance timestamp, expiry timestamp.

Revisions are canonical positive unsigned decimal integers; timestamps and
credential IDs are canonical decimal integers in Unix seconds and storage ID
units respectively. Operation digests are `sha256:` plus 64 lowercase hex digits.
IDs and the configured issuer are nonempty ASCII of at most 256 bytes using only
`[A-Za-z0-9._:@/-]`; this excludes invisible formatting and bidi controls.

Possession uses the same length framing with domain
`tether-enrollment-possession-v2`, the same eight binding fields, then action,
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


## Reviewed hardening and v2 framing

Custody profile `tether-enrollment-custody-v2` fixes the suite above, including
Ed25519 issuer signatures. The issuer field changes the transcripts, so v1
custody proofs/envelopes are refused. This is separate from the proposed closed
`privileged-operation-v2-draft`, which must require both key thumbprints and this
custody profile along with the already-required destination ID/revision.

Signing public keys must decode canonically and be nonidentity prime-order
points. Using `filippo.io/edwards25519`, validation clears the cofactor, multiplies
by inverse-eight modulo the prime order, and requires equality with the original
point. It rejects pure torsion and mixed-order keys. The underlying package's
`SetBytes` accepts noncanonical encodings, so an explicit canonical roundtrip is
also required. X25519 validation uses a fixed public scalar only to reject
noncontributory keys; it is never used for encryption.

Unsigned `seal`/`open` are package-private. `OpenSigned` is the only exported
opening path. Its `SignedDelivery` has the exact signature profile
`tether-enrollment-delivery-v2+sig`, key ID, complete delivery and rawbase64url
Ed25519 signature. Signature bytes use that domain plus NUL, then the same eight
binding fields followed by key ID, inner delivery profile, credential ID,
issuance timestamp, expiry timestamp and ciphertext, with the same uint32 length
framing. The ciphertext and key ID are authenticated as well as all metadata.
The signature profile is fixed, not algorithm negotiation.

The helper must supply trust from its pinned issuer loader, never from an
untrusted response: enabled enrollment, allowed key ID, JWKS successfully checked
less than 60 seconds ago and emergency controls checked less than five seconds
ago. Unknown/denied keys, stale/missing/future snapshots and wrong issuers fail
closed. A refresh failure must invalidate the snapshot. No network trust loader
or emergency configuration reader is implemented here. The issuer signer remains
private and unwired; a future controller must load approved state and the existing
issuer key internally, with no public arbitrary-envelope signing surface.

Only destination-side Sign/OpenSigned allow issuance up to five seconds ahead
of the destination clock. Server-side challenge verification allows no future
issuance. No side allows any expiry grace. Trust freshness is measured locally
and has no positive skew allowance.

Before storing or confirming, the helper must privately exchange the delivered
credential through the ordinary issuer, verify that JWT against pinned current
trust, and require the exact approved principal and scopes. A valid delivery
signature alone does not establish credential validity. This exchange check and
the destination helper remain unimplemented activation requirements. Show both
full thumbprints in the human ceremony and helper; no short authentication string
is introduced. Base64url thumbprints are case-sensitive, and uppercase letters
are valid alphabet members.
