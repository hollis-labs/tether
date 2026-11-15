# Operator-attested replies (contract)

Status: **contract, revision 2, approved** by @archie and @photon-review. Built
in CW-20261114-0152; see "Build notes" at the end.
Reviewers are @archie and @photon-review. Builders are Tether (this repo),
Tangent (the signer, CW-20261114-0153) and Tachyon (the client).

@archie decided on 2026-11-14: option A, a Tangent-signed Ed25519 assertion
over a single-use Tether challenge. Revision 2 applies @photon-review's
security review and @archie's answers:

- Tangent gets what it displays from Tether, never from Tachyon.
- One step-up authorizes exactly one assertion.
- The trust root is a privileged, operator-approved catalog field.
- `already_answered` is re-checked at reply time.
- A challenge is consumed by any verification attempt.
- The freshness and binding rules are tightened.

## Purpose

Chrispian answers a typed card (`question`, `decision` or `approval_request`)
from Tachyon's UI, and Tether stores it as **his verified reply**:
`from_principal` is the operator, `from_verification` is `verified`, and an
`operator_attestation` records how it was proven. Tachyon holds only an
operator-paired device credential and can't vouch for him by itself. A fresh
human step-up must be **provable to Tether**, and the proof comes from Tangent,
which enforces the step-up.

What this rules out:

- **The device saying so.** A compromised Tachyon device must not be able to
  forge, multiply, retarget or replay his answers. Tachyon's own elevation
  (mirrored from Tangent, no id, default lifetime `always`) is never proof.
- **`on_behalf_of`.** That is a claim, triaged `action`, and never an approval
  (docs/typed-messages.md).
- **Stale proofs.** The freshness bound is Tether's own and holds whatever the
  operator's `elevation_ttl` is.

## Parties and trust

| Party | Holds | Trusted for |
|---|---|---|
| Tachyon | an operator-paired device credential with scope `message.operator_reply` | asking for a challenge and forwarding the answer. **Nothing it says is shown to Chrispian as fact or signed on its word.** |
| Tangent | an Ed25519 signing key; enforces Chrispian's step-up; a Tether credential with scope `message.operator_reply_view` | showing Chrispian what Tether says he is answering, and attesting that he stepped up and chose it |
| Tether | challenges, the record of used `jti` and `elevation_id` values, **Tangent's public keys in a privileged catalog field** | the canonical card, verification, replay refusal, stamping and storing |

Tangent is fully trusted by design: a compromised Tangent can sign anything.
See "Out of scope".

## Catalog: the trust root (privileged)

```yaml
# global.yaml
operator_attestation:
  issuer: tangent              # must equal the assertion's iss
  max_age_seconds: 300         # freshness bound, at most 300; independent of elevation_ttl
  keys:                        # at most 2 entries (rotation overlap)
    - id: tangent-2026-11      # the JWS kid
      public_key: ed25519:<base64url of the 32-byte key>
```

- **Privileged field.**
  - Any plan that adds, changes or removes `operator_attestation` is a
    privileged plan (reason "changes the operator attestation trust root"). It
    is applied only through `tether catalog plan` followed by
    `tether catalog approve --ack-privileged`, and **only by the operator
    credential**, even where delegated approval is enabled for other plans.
  - Tether records the digest of the approved block when it applies one.
  - A plain catalog **reload** that finds a block different from the last
    approved digest fails that block's validation: the reload is refused, the
    previous keys stay in force, and the refusal is audited. That stops a hand
    edit or any write path outside the privileged plan from changing the trust
    root.
  - Every change is recorded in `identity_audit` with
    `event = operator_attestation_keys_changed` and the old and new key ids.
- With no approved `operator_attestation`, the feature is off and every route
  below answers 404 `feature_disabled`.
- **Rotation**, each step through the privileged path:
  1. Add the new key as a second entry.
  2. Tangent starts signing with the new `kid`.
  3. Once `max_age_seconds` plus 60 s have passed, remove the old entry.

  More than two keys is a validation error.

## Scopes

- **`message.operator_reply`**: a new **device** scope, grantable only in a
  pairing grant the operator creates. It allows the challenge and reply routes,
  and only when the principal is verified, `kind = device`, not an issued token
  (`TokenID == ""`), and paired by the operator
  (`created_by = msg://user/local/operator`).
- **`message.operator_reply_view`**: a new scope for Tangent's own Tether
  credential, an operator-paired device for Tangent. It allows only the view
  route below.
- Anything else is refused: 403 `not_operator_device`, or `scope_required` when
  the device lacks the scope.

## Flow

```
Tachyon                         Tether                               Tangent
   | POST .../operator-reply/challenge {option}                         |
   |------------------------------>| checks parent + option; stores     |
   |   {challenge_id, nonce, ...}  | challenge (300 s, single use)      |
   |<------------------------------|                                    |
   | ask Tangent to sign challenge_id (+ Chrispian's note, if any)      |
   |--------------------------------------------------------------->   |
   |                               | GET .../challenges/{challenge_id}  |
   |                               |<-----------------------------------|
   |                               | canonical card + binding hash      |
   |                               |----------------------------------->|
   |                     Chrispian sees Tether's card; steps up once;   |
   |                     Tangent signs this one challenge               |
   |<---------------------------------------------------------------    |
   | POST .../operator-reply {challenge_id, option, note?, assertion}   |
   |------------------------------>| consumes challenge; verifies;      |
   |   201 {message_id, ...}       | stores the reply; audits           |
   |<------------------------------|                                    |
```

### 1. Challenge: `POST /messages/{id}/operator-reply/challenge` (Tachyon)

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
 "message_id": "<id>", "option": "<option>",
 "binding": "<hex sha256(message_id || 0x00 || option)>",
 "expires_at": "<now + 300 s>"}
```

- A challenge lasts `max_age_seconds` (300 s).
- At most **10** challenges may be outstanding per device; more is 429
  `too_many_challenges`.
- A challenge is **single-use**: it is consumed by the first reply attempt that
  names it, **whether that attempt passes or fails**.
- Stored: `{challenge_id, sha256(nonce), message_id, option, binding,
  device_id, created_at, expires_at, consumed_at}`. It's a new table; the
  migration number is assigned at build time.

### 2. View: `GET /operator-attestation/challenges/{challenge_id}` (Tangent)

Tangent calls this with its own credential (scope
`message.operator_reply_view`). It displays **only** what this returns, never
anything Tachyon supplies about the card. The challenge must be open (not
expired, not consumed).

```json
{"challenge_id": "orc_…", "message_id": "<id>", "binding": "<hex>",
 "kind": "approval_request",
 "asker": {"handle": "@archie", "address": "msg://agent/…", "from_verification": "verified"},
 "subject": "Ship #562?",
 "option": {"id": "approve", "label": "Approve", "detail": "…"},
 "excerpt": "<first ~500 characters of the body>",
 "excerpt_truncated": true,
 "body_sha256": "<hex>",
 "full_url": "<Tether URL for the full body, same auth>"}
```

**What Tangent's step-up prompt shows**, as decided by @archie:

- the kind;
- the asker's handle **with its `from_verification`**: an unverified asker is
  shown as such, because the answer goes to the parent's sender;
- the subject;
- the chosen option's label (and detail);
- the body excerpt, about 500 characters, with a "view full" link to
  `full_url`;
- for an `approval_request`, the body's impact and rollback, which are part of
  the body the excerpt and the full view show;
- the note, if Chrispian wrote one. Tangent computes `note_sha256` from the text
  it shows.

What gets signed is the **binding** (hash of the parent id and the option id),
so truncating the display can't change what is approved.

### 3. Assertion (Tangent's side)

Tangent signs a compact JWS (`alg: EdDSA`, `kid` = a catalog key id,
`typ: operator-reply+jwt`) only after Chrispian completes a step-up **for this
challenge**, within the last 300 s, whatever `elevation_ttl` says. **One step-up
authorizes exactly one assertion for one challenge**, for every kind. It can't
be reused within the window.

| Claim | Value |
|---|---|
| `iss` | `tangent` (the catalog `issuer`) |
| `aud` | `tether` |
| `sub` | `msg://user/local/operator` |
| `jti` | unique per assertion (UUID) |
| `iat`, `exp` | `exp - iat <= 300` |
| `challenge_id`, `nonce` | exactly as Tether issued them |
| `message_id`, `option` | as the view returned them |
| `binding` | as the view returned it: hex `sha256(message_id \|\| 0x00 \|\| option)` |
| `body_sha256` | as the view returned it |
| `note_sha256` | hex SHA-256 of the note Chrispian saw, **present if and only if there is a note** |
| `device_id` | the Tachyon device the challenge was issued to (from the view's challenge record) |
| `elevation_id` | Tangent's id for this step-up (unique) |
| `elevated_at` | Unix time the step-up completed |

### 4. Reply: `POST /messages/{id}/operator-reply` (Tachyon)

Request: `{"challenge_id": "...", "option": "...", "note": "...?", "assertion": "<JWS>"}`

Tether verifies in this order and refuses on the first failure. **Nothing is
stored on any failure, except that the challenge stays consumed.**

1. The principal is an operator device with the scope (see Scopes).
2. The challenge exists, was issued to **this** device, and belongs to this
   `{id}`. It is then **consumed** (`UPDATE … SET consumed_at=now WHERE
   consumed_at IS NULL`). If it was already consumed, the result is 409
   `replayed`; if it has expired, 400 `challenge_expired`. Every later step
   works on a consumed challenge.
3. The JWS signature verifies with the approved catalog key named by `kid`, and
   `alg` is exactly `EdDSA`.
4. Freshness, with skew = 30 s:
   - `exp > now`;
   - `exp - iat <= max_age_seconds`;
   - `iat <= now + skew`;
   - `now - iat <= max_age_seconds`;
   - `elevated_at <= now + skew`;
   - `now - elevated_at <= max_age_seconds`;
   - `elevated_at <= iat`.
5. Binding:
   - `iss`, `aud` and `sub` are as above;
   - `challenge_id`, `nonce` (compared through its stored hash), `message_id`,
     `option`, `binding`, `body_sha256` and `device_id` match the challenge,
     the parent and the request;
   - `option` in the request equals the challenge's option;
   - `note_sha256` is present **if and only if** the request has a note, and
     then matches it.
6. One-use proofs: neither the `jti` nor the `elevation_id` has been used
   before.
7. In **one transaction**:
   - re-check that the operator hasn't already answered the parent (another
     challenge may have succeeded meanwhile; if so, 409 `already_answered`);
   - record the `jti` and the `elevation_id`;
   - store the reply.

The stored reply is a typed envelope of kind `message`, with
`in_reply_to = {id}` and `answer = {option, note?}`, sent to the parent's
sender. It's stamped as follows:

- `from`: the operator's canonical address (#554);
- metadata `from_principal`: `msg://user/local/operator`;
- metadata `from_verification`: `verified`;
- metadata `operator_attestation`: a JSON string
  `{"device_id","elevation_id","elevated_at","assertion_id":jti,"challenge_id","key_id":kid}`.

It then follows normal mail: the parent settles in `GET /messages/pending-replies`,
and triage classes it `from_operator` (#564).

**Audit.** Every reply attempt, accepted or refused, is recorded in
`identity_audit` (`event = operator_reply_attested` or `operator_reply_refused`).
The row carries the device, the parent, the challenge, the `elevation_id`, the
refusal code, and the parent sender's `from_verification`. That last field
matters because the answer goes to the parent's sender as stored, so an
unverified asker is visible both in Tangent's prompt and in the audit.

## Errors

| Code | HTTP | When |
|---|---|---|
| `feature_disabled` | 404 | no approved `operator_attestation` in the catalog |
| `not_operator_device` | 403 | not a verified, operator-paired, non-token device |
| `scope_required` | 403 | the device lacks the route's scope |
| `parent_not_found` | 404 | no such message |
| `parent_not_operators` | 403 | the parent isn't addressed to the operator |
| `not_answerable` | 400 | the parent isn't a question, decision or approval_request (`{field: "in_reply_to"}`) |
| `invalid_option` | 400 | the option isn't one of the parent's (`{field: "answer.option", reason}`) |
| `already_answered` | 409 | the operator already answered the parent, checked at challenge time and again in the reply transaction |
| `challenge_unknown` | 400 | no such challenge, or it belongs to another device or message |
| `challenge_expired` | 400 | the challenge has expired (it is consumed anyway) |
| `assertion_invalid` | 401 | malformed JWS, wrong alg, unknown or unapproved kid, or a bad signature |
| `assertion_stale` | 401 | any freshness rule in step 4 fails |
| `assertion_unbound` | 401 | any binding rule in step 5 fails, including a note present without `note_sha256` or the reverse |
| `replayed` | 409 | the challenge is already consumed, or the `jti` or `elevation_id` was already used |
| `too_many_challenges` | 429 | more than 10 outstanding challenges for the device |

## What a compromised device can still do

These are the residual risks once Tangent behaves as specified:

- request challenges: at most 10 outstanding, each living 300 s;
- start a step-up prompt that **shows Tether's canonical card**, which
  Chrispian can decline;
- drop or delay a reply.

It **cannot**:

- forge an assertion;
- replay one (single-use challenge, `jti` and `elevation_id`);
- swap the parent or the option (bound and displayed from Tether);
- slip a note past him (he sees it and its hash is bound);
- turn one step-up into several answers;
- answer a card twice.

## What each builder owns

- **Tether** (CW-20261114-0152):
  - the privileged `operator_attestation` block, its approved-digest check on
    reload, and its audit;
  - both scopes;
  - the challenge, view and reply routes;
  - the challenge, `jti` and `elevation_id` stores;
  - verification, stamping and audit;
  - docs and tests: every error row, `elevation_ttl = always` not making a
    6-minute-old step-up acceptable, a consumed-on-failure challenge,
    `elevation_id` reuse, the reply-time `already_answered`, and a
    non-privileged reload refused.
- **Tangent** (CW-20261114-0153):
  - the signing key and publishing its public key;
  - its operator-paired Tether credential with `message.operator_reply_view`;
  - fetching the card from Tether's view route and showing only that;
  - a step-up per challenge, at most 300 s old regardless of `elevation_ttl`
    and spent on one signature;
  - the assertion as specified;
  - a test vector for Tether's tests.
- **Tachyon** (CW-20261114-0023 e): the answer buttons, calling challenge →
  Tangent (`challenge_id`, plus the note text) → reply, and showing the error
  codes.

## Out of scope

- WebAuthn or passkeys verified by Tether (option B), and device-key signatures
  (option C, rejected).
- **A compromised Tangent.** Tangent is the attesting party and is fully
  trusted by design: a compromised Tangent, or a stolen Tangent signing key,
  can forge answers until its key is removed through the privileged catalog
  path. Keep the key on the Tangent host and rotate it after any suspicion.
- Answering anything other than the typed kinds above.
- Changing `on_behalf_of`.
- Issuing operator credentials to Tachyon.

## Build notes

Where the build had to choose, and what it chose:

- **Step 7's transaction.** The message store cannot join an outside
  transaction, so the reply itself is stored just after it. One transaction
  re-checks `already_answered`, records the `jti` and `elevation_id` (both
  `UNIQUE`, the replay guard against concurrent replies) and reserves the
  parent's one answer (`operator_reply_answers`, keyed by the parent). The
  reply is then sent. If sending fails, only the reservation is released; the
  proofs stay spent. Every guard is in the transaction.
- **The approved digest** is stored in the state database
  (`operator_attestation_trust`, migration 0106), outside the catalog root a
  plan writes to. It is the SHA-256 of the block's canonical JSON. A reload with
  any other block, including a hand-removed one, is refused; at start such a
  block leaves the feature off. An approved change takes effect on reload, with
  no restart.
- **Audit detail.** `identity_audit` gains a `detail` JSON column for the
  fields these rows carry (device, parent, challenge, elevation, refusal code,
  parent `from_verification`; old and new key ids).
- **`full_url`** is `GET /operator-attestation/challenges/{id}/body`, under
  the same view scope.
- **The reply** is a typed `message` from `@chrispian` to the asker's handle
  (or address when it has none), with `answer {option, note?}`, the option's
  label as its body, and `Re: <subject>`.
