# Operator-attested replies (contract)

Status: **contract for review** (CW-20261114-0152). No code yet. Reviewers:
@archie and @photon-review. Builders: Tether (this repo), Tangent (the signer)
and Tachyon (the client). Decided by @archie on 2026-11-14: option A, a
Tangent-signed Ed25519 assertion over a single-use Tether challenge.

## Purpose

Chrispian answers a typed card (`question`, `decision` or `approval_request`)
from Tachyon's UI, and Tether stores the answer as **his verified reply**:
`from_principal` is the operator, `from_verification` is `verified`, and an
`operator_attestation` records how it was proven. Tachyon holds only an
operator-paired device credential, so the device can't vouch for him by
itself. A fresh human step-up must be **provable to Tether**, and that proof
comes from Tangent, which enforces the step-up.

What this rules out:

- **A device saying so.** A compromised Tachyon device must not be able to
  forge his answers. Tachyon's own elevation (mirrored from Tangent, no id,
  default lifetime `always`) is never accepted as proof.
- **`on_behalf_of`.** That is a claim, triaged `action`, and never an approval
  (docs/typed-messages.md).
- **Stale proofs.** The freshness bound below is Tether's own and applies
  whatever the operator's `elevation_ttl` is.

## Parties and trust

| Party | Holds | Trusted for |
|---|---|---|
| Tachyon | an operator-paired device credential with scope `message.operator_reply` | asking for a challenge and forwarding the answer; nothing about Chrispian himself |
| Tangent | an Ed25519 signing key; enforces Chrispian's step-up | attesting "the operator stepped up at T and chose option O for message M under challenge C" |
| Tether | the challenge store; **Tangent's public keys, pinned in the catalog** | verifying the assertion, refusing replay, stamping and storing the reply |

The trust root is the Tangent public key in Tether's **catalog**: git-tracked
and reviewable. It is never taken from a pairing record or a request.

## Catalog

```yaml
operator_attestation:
  issuer: tangent              # must equal the assertion's iss
  max_age_seconds: 300         # freshness bound; at most 300; independent of elevation_ttl
  keys:                        # at most 2 entries (rotation overlap)
    - id: tangent-2026-11      # the JWS kid
      public_key: ed25519:<base64url of the 32-byte key>
```

- With no `operator_attestation` block, the feature is off and every route
  below answers 404 `feature_disabled`.
- **Rotation:**
  1. Add the new key as a second entry and reload the catalog.
  2. Tangent starts signing with the new `kid`.
  3. Once `max_age_seconds` plus 60 s have passed, remove the old entry.

  More than two keys is a catalog validation error.

## Scope

`message.operator_reply` is a new **device** scope. It is added to the device
scope vocabulary and is grantable only in a pairing grant the operator creates.
A request uses the routes below only when **all** of these hold:

- the principal is verified, `kind = device`, and not an issued token
  (`TokenID == ""`);
- the device was paired by the operator (`created_by = msg://user/local/operator`);
- it holds `message.operator_reply`.

Sessions, agents, issued tokens, and worker-only or foreign-paired devices are
refused (403 `not_operator_device`), and so is an operator-paired device
without the scope (403 `scope_required`).

## Flow

```
Tachyon                         Tether                              Tangent
   | POST .../operator-reply/challenge {option}                        |
   |------------------------------>| checks parent + option; stores    |
   |   {challenge_id, nonce, ...}  | challenge (5 min, single use)     |
   |<------------------------------|                                   |
   |   ask Chrispian; Tangent step-up (fresh, <= 300 s)                |
   |--------------------------------------------------------------->  |
   |                       JWS assertion over the challenge            |
   |<---------------------------------------------------------------   |
   | POST .../operator-reply {challenge_id, option, note?, assertion}  |
   |------------------------------>| verifies; consumes challenge;     |
   |   201 {message_id, ...}       | stores the reply; audits          |
   |<------------------------------|                                   |
```

### 1. Challenge: `POST /messages/{id}/operator-reply/challenge`

Request: `{"option": "<option id>"}`, with `id` the parent message.

Tether checks:

- the parent exists and is a typed envelope of kind `question`, `decision` or
  `approval_request`;
- it is addressed to one of the operator's forms (`msg://user/local/operator`
  or an alias of it, #554);
- `option` is one of the parent's option ids (or `approve` / `deny` for an
  `approval_request` without options);
- the operator hasn't already answered it.

Response `201`:

```json
{"challenge_id": "orc_<uuid>", "nonce": "<base64url, 32 random bytes>",
 "message_id": "<id>", "option": "<option>", "expires_at": "<now + max_age_seconds>"}
```

Tether stores `{challenge_id, sha256(nonce), message_id, option, device_id,
created_at, expires_at, consumed_at}`. It's a new table; the migration number
is assigned at build time. A device may hold several open challenges, but at
most 10 per device: more is 429 `too_many_challenges`.

### 2. Assertion (Tangent's side)

Tangent signs a compact JWS (`alg: EdDSA`, `kid` = a catalog key id,
`typ: operator-reply+jwt`) **only after a step-up completed within the last
`max_age_seconds` (300 s)**, regardless of the operator's `elevation_ttl`. For
an approval this normally means a step-up for this very answer.

| Claim | Value |
|---|---|
| `iss` | `tangent` (the catalog `issuer`) |
| `aud` | `tether` |
| `sub` | `msg://user/local/operator` |
| `jti` | unique per assertion (UUID) |
| `iat`, `exp` | `exp - iat <= 300` |
| `challenge_id`, `nonce` | exactly as Tether issued them |
| `message_id`, `option` | the parent and the chosen option |
| `note_sha256` | hex SHA-256 of the note, or absent when there is no note |
| `device_id` | the Tachyon device principal id the challenge was issued to |
| `elevation_id` | Tangent's id for the step-up |
| `elevated_at` | Unix time the step-up completed |

### 3. Reply: `POST /messages/{id}/operator-reply`

Request: `{"challenge_id": "...", "option": "...", "note": "...?", "assertion": "<JWS>"}`

Tether verifies the following, in order, and refuses on the first failure.
Nothing is stored on any failure.

1. The principal is an operator device with the scope (see Scope).
2. The challenge exists, was issued to **this** device, belongs to this `{id}`
   and `option`, isn't expired, and isn't consumed.
3. The JWS signature verifies with the catalog key named by `kid`.
   `alg` is exactly `EdDSA`.
4. Claims:
   - `iss`, `aud` and `sub` are as above;
   - `exp > now`;
   - `iat <= now + 30 s` (skew);
   - `now - iat <= max_age_seconds`;
   - `now - elevated_at <= max_age_seconds`;
   - `elevated_at <= iat`.
5. Binding: `challenge_id`, `nonce` (compared through its hash), `message_id`,
   `option`, `device_id` and `note_sha256` all match the request and the
   challenge.
6. `jti` has never been used.
7. In one transaction: consume the challenge (`consumed_at IS NULL` guard),
   record the `jti`, and store the reply. A lost race is 409 `replayed`.

The stored reply is a typed envelope of kind `message`, with
`in_reply_to = {id}` and `answer = {option, note?}`, sent to the parent's
sender. It's stamped as follows:

- `from`: the operator's canonical address (#554);
- metadata `from_principal`: `msg://user/local/operator`;
- metadata `from_verification`: `verified`;
- metadata `operator_attestation`: a JSON string
  `{"device_id","elevation_id","elevated_at","assertion_id":jti,"challenge_id","key_id":kid}`.

It then follows normal mail: the parent settles in `GET /messages/pending-replies`,
and triage classes it `from_operator` because the sender is verified (#564).
An `identity_audit` row records `event = operator_reply_attested` with the
device, the parent, the challenge and the elevation.

## Errors

| Code | HTTP | When |
|---|---|---|
| `feature_disabled` | 404 | no `operator_attestation` in the catalog |
| `not_operator_device` | 403 | not a verified, operator-paired, non-token device |
| `scope_required` | 403 | the device lacks `message.operator_reply` |
| `parent_not_found` | 404 | no such message |
| `parent_not_operators` | 403 | the parent isn't addressed to the operator |
| `not_answerable` | 400 | the parent isn't a question, decision or approval_request (`{field: "in_reply_to"}`) |
| `invalid_option` | 400 | the option isn't one of the parent's (`{field: "answer.option", reason}`) |
| `already_answered` | 409 | the operator already answered the parent |
| `challenge_unknown` / `challenge_expired` | 400 | the challenge doesn't exist, belongs to another device or message, or has expired |
| `assertion_invalid` | 401 | malformed JWS, wrong alg, unknown kid, or a bad signature |
| `assertion_stale` | 401 | `iat` or `elevated_at` is older than `max_age_seconds`, or `exp` has passed |
| `assertion_unbound` | 401 | a claim doesn't match the challenge or request (nonce, message, option, device, note) |
| `replayed` | 409 | the challenge is already consumed, or the `jti` was already used |
| `too_many_challenges` | 429 | more than 10 open challenges for the device |

## What each builder owns

- **Tether** (CW-20261114-0152): the catalog block and its validation, the
  device scope, both routes, the challenge and jti store, verification,
  stamping, audit, docs and tests. Tests cover every row of the error table,
  plus `elevation_ttl = always` not making a 6-minute-old step-up acceptable.
- **Tangent** (separate task): the signing key, publishing the public key for
  the catalog, the step-up freshness rule (≤ 300 s regardless of
  `elevation_ttl`), and an endpoint Tachyon calls to get an assertion for a
  challenge. Tangent must show Chrispian the parent's subject and the chosen
  option in the step-up prompt, so he knows what he is signing.
- **Tachyon** (CW-20261114-0023 e): the answer buttons, calling challenge →
  Tangent → reply, and showing the error codes.

## Out of scope

- WebAuthn or passkeys verified by Tether (option B), and device-key signatures
  (option C, rejected).
- Answering anything other than the typed kinds above.
- Changing `on_behalf_of`.
- Issuing operator credentials to Tachyon.
