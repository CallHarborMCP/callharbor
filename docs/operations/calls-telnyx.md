# Calls and Telnyx setup

This guide covers what an operator must set for outbound calls, what the server
refuses when it is missing, and how to roll back safely. The REST contract is in
[api.md](api.md#placing-calls). The MCP `make_call` tool is in
`api/mcp/tools.json`.

## Required settings

| Setting | Required | Purpose |
| --- | --- | --- |
| `CALLHARBOR_PUBLIC_URL` | Yes | The public origin Telnyx calls back. Must be `https` in production. |
| `CALLHARBOR_MASTER_KEY` or `CALLHARBOR_MASTER_KEY_FILE` | Yes | Secrets key. Stored Telnyx keys, including the webhook public key, are encrypted with it. Stream URL tokens are derived from it. |
| `CALLHARBOR_MAX_CALL_BRIEF_BYTES` | No | Limit on the canonical brief, default 32 KiB, bounds 1 KiB to 256 KiB. A larger brief is refused with `brief_too_large`. |

### Call safety settings

These are installation-wide limits. They are read once at startup, so a change
takes effect after a restart. Per-user settings (below) can only tighten them.

| Setting | Default | Purpose |
| --- | --- | --- |
| `CALLHARBOR_CALL_ALLOW_PREFIXES` | unset (all allowed) | Comma-separated E.164 prefixes. When set, a destination must match one. |
| `CALLHARBOR_CALL_DENY_PREFIXES` | unset | Comma-separated E.164 prefixes that are always refused. Deny wins over allow. |
| `CALLHARBOR_MAX_CONCURRENT_CALLS` | `10` (1 to 1000) | Installation-wide limit on non-terminal calls: queued, initiating, ringing, active and ending. |
| `CALLHARBOR_USER_MAX_CONCURRENT_CALLS` | `3` (1 to 1000) | Default per-user limit on non-terminal calls. |
| `CALLHARBOR_USER_MAX_CALLS_PER_HOUR` | `30` (1 to 10000) | Default per-user limit on calls created in the last hour. |
| `CALLHARBOR_MAX_CALL_DURATION_SECONDS` | `7200` (30 to 7200) | Installation maximum for `max_duration_seconds`. |
| `CALLHARBOR_RING_TIMEOUT_SECONDS` | `30` (5 to 600) | How long Telnyx rings the callee before giving up. Sent as the dial's timeout. |
| `CALLHARBOR_MAX_CALL_COST` | unset (no cost limit) | Optional per-call cost limit as a decimal, such as `1.50`. |
| `CALLHARBOR_CALL_COST_ESTIMATE_PER_MINUTE` | unset | Optional operator-supplied rate, as a decimal, used only for cost estimates. CallHarbor has no built-in tariff. |
| `CALLHARBOR_CALL_COST_CURRENCY` | unset | ISO 4217 code (three uppercase letters). Required when either cost setting is set. |

Limits count database rows, so a call that ends frees its slot as soon as its
state is terminal. The concurrency and rate checks run in the same transaction
that writes the call, so they cannot be raced past.

A cost limit with no estimate rate fails closed: the call is refused with
`cost_unavailable`, because its cost cannot be checked. Without a cost limit, a
configured rate is recorded as an estimate and no rate means the cost is
unavailable. The call is still placed.

### Refusals

A refused call writes no call row. Its error code is one of the envelope codes
(`forbidden`, `invalid_input`, `conflict` or `rate_limited`), and the message
begins with one stable reason code, followed by a colon and a short explanation,
so clients can match on the prefix. Messages never contain full phone numbers or
keys. The [API guide](api.md#placing-calls) lists the HTTP status for each.

| Reason code | Meaning |
| --- | --- |
| `destination_denied` | The destination matches a deny prefix, or the user's deny list. |
| `destination_not_allowed` | An allow list is set and the destination matches none of its prefixes. |
| `max_duration_exceeds_policy` | An explicit `max_duration_seconds` is above the effective policy maximum. An omitted duration uses the effective maximum instead. |
| `cost_unavailable` | A cost limit is set but no estimate rate exists, so the cost cannot be checked. |
| `cost_limit_exceeded` | The estimate is above the per-call cost limit. |
| `concurrency_limit` | The user already has the maximum number of non-terminal calls. |
| `global_concurrency_limit` | The installation already has the maximum number of non-terminal calls. |
| `rate_limited` | The user has placed the maximum number of calls in the last hour. |

The worker can also refuse a queued call at dial time with `policy_unavailable`.
That is a fail-closed check: if the policy cannot be read, the call is failed
without a dial.

### Per-user policy

An administrator sets a user's policy with `PUT /api/v1/users/{user_id}/call-policy`
and reads it with `GET /api/v1/users/{user_id}/call-policy`. A user reads their own
effective policy with `GET /api/v1/call-policy`. Only administrators can write.

A user's policy can only tighten the installation settings. Each limit takes the
stricter of the installation value and the user's value, so a user value above the
installation limit has no effect. Deny prefixes from both layers apply. An allow
list from each layer must match, so a user allow list narrows the installation
allow list and cannot widen it. The installation policy is static environment
configuration: it does not change on a policy write, only on a restart.

The call worker runs only when `CALLHARBOR_PUBLIC_URL` is set **and** the master
key is present. Without either, calls are disabled and the server still starts,
so OAuth and MCP keep working. `POST /api/v1/calls` and `make_call` then return
`503 unavailable` with reason `calls_unavailable`, and no call row is written.
At startup the log says `calls are disabled until CALLHARBOR_PUBLIC_URL is set`
or `calls are disabled: they need the secrets master key`.

## Telnyx URLs

The server derives these from `CALLHARBOR_PUBLIC_URL`. Configure nothing else in
Telnyx beyond the connection and its webhook URL. Telnyx call recording,
transcription, answering-machine detection (AMD), AI assistants and other paid
add-ons must stay disabled on the number and the Call Control application.
CallHarbor never enables them and records locally only.

| Use | URL |
| --- | --- |
| Call-control webhook, per call | `{PUBLIC_URL}/telnyx/calls/{call_id}/webhook` |
| Media stream, per call | `wss://…/telnyx/calls/{call_id}/media/{token}` (derived from `https`; `http` becomes `ws`) |

The stream token is derived from the master key, not stored. Changing
`CALLHARBOR_MASTER_KEY` or `_FILE` invalidates every in-flight stream URL and
disables calls until the new key is in place.

## Telephony profile readiness

A new call needs a telephony profile that is:

1. `connectivity_status` `verified` (run `POST /api/v1/telephony-profiles/{id}/validate`),
2. with a `connection_id`, and
3. with a `webhook_public_key`.

Otherwise the call is refused with `telephony_not_verified`,
`telephony_connection_missing` or `telephony_webhook_key_missing`.

The webhook public key is stored encrypted with the master key, not derived from
it, and it is resolved from the telephony profile owner's secrets, not the
grantee's. Each dial sends its own webhook URL, so the server does not depend on
one webhook URL configured in Telnyx.

## Grants

An owner can grant a user use of a telephony profile. The grantee places calls
that the owner's key dials, and the owner's key is used for hangup at shutdown.
Each user must have exactly one AI profile to place a call, because the server
selects the AI profile from the caller's own profiles and no request field can
choose it.

## Shutdown

On shutdown the server stops taking new dials, lets an in-flight dial finish up
to `CALLHARBOR_SHUTDOWN_TIMEOUT`, then sends a best-effort hangup for each live
leg. A hangup that fails is logged and does not block shutdown. A dial cut off by
shutdown is recorded as `dial_unconfirmed`, never redialed.

The shutdown hangup pass covers live calls only. A leg found only after its call
was already failed (for example, a dial whose outcome was unknown) is hung up when
its webhooks arrive, and again on each later event until Telnyx reports the
hangup. If those attempts fail and no further event arrives, Telnyx's
`time_limit_secs` is the backstop. Restart and sweep reconciliation (below) covers
the same case in the server itself.

The container's stop grace period must cover the worst-case shutdown sequence.
With `T` set to `CALLHARBOR_SHUTDOWN_TIMEOUT` and `B = max(T/3, 5 s)`, the sum
is `T + 5 s + 3B`: 35 s at the default 15 s, and 605 s at the 5 m maximum. At
the default, use `docker stop --time 40` or Compose `stop_grace_period: 40s`.
See `docs/operations/docker.md`.

Dials are serial: one worker places one call at a time, so a slow dial delays
the dials queued behind it. Head-of-line blocking is not addressed: a dial that
takes its full ring timeout holds back every queued call behind it.

## Ending a call from CallHarbor

CallHarbor ends a live call itself in three cases:

| Reason (`outcome_reason_code`) | When |
| --- | --- |
| `max_duration` | The call's `max_duration_seconds` expired. |
| `media_failed` | The AI session failed mid-call (a provider error or a dropped connection). |
| `ai_session_closed` | The AI session closed cleanly while the caller was still connected. |
| `caller_silence` | No caller speech and no agent audio for `callstream.SilenceTimeout` (30 s). The window starts when the caller's last speech ended or when the agent's last queued audio finished, whichever is later, so a long agent reply does not count toward it, and a pause after a reply is measured from the reply's end. CallHarbor closes the AI session and hangs up the leg. Caller speech is detected by a local energy detector. Line noise that the detector reads as speech counts as caller speech, but only for a bounded time: a caller turn that stays open longer than 15 s (`MaxCallerTurn`) is force-ended and the detector is reset, so noise that persists past that bound no longer holds the call active and this reason can fire. Noise that breaks up into short bursts can still reset the silence window within that bound. The detector is live-unverified. |

When one of these happens, the call moves to `ending` and one `call.ending`
event is recorded. Then CallHarbor sends one best-effort Telnyx hangup for the
call's control id, with the owner's key. The hangup's command id is derived
from the call id, so a retry is the same command and Telnyx treats it as one.

The call is `ended` only when Telnyx's signed `call.hangup` webhook arrives. If
the hangup request fails, the failure is logged without the key, and the call
stays in `ending`. The Telnyx `time_limit_secs` of the dial is the outer backstop:
it is sent as the cap clamped to Telnyx's accepted range (30 to 14400 seconds),
so Telnyx ends the leg even if CallHarbor's hangup never reaches it. Because the
clamp can be larger than the per-call cap, the per-call cap is enforced by
CallHarbor, and the Telnyx limit is the outer bound.

A call whose AI session fails before the stream starts is ended the same way.
A stream closed by Telnyx, or a caller who has left, is not hung up again.

### Emergency hangup

The call's owner can end their own call, through
`POST /api/v1/calls/{call_id}/hangup` or the MCP `hangup_call` tool. An
administrator can end any call, but only through the REST route: MCP actors are
never administrators, so the MCP tool ends only the caller's own calls. The REST
route needs a session and the CSRF token, and the MCP tool needs the `mcp:call`
scope. A repeat request is safe: it returns the current state. For a call still
`ending` it may send the hangup again, with the same command id each time.

Refusals differ in what they record, so the audit trail is exact:

- A caller who does not own the call and is not an administrator gets `not_found`.
  That refusal writes one `call.hangup` audit row with outcome `denied` and no
  target, so the owner is not revealed.
- A missing call (`not_found`), an invalid `call_id`, a missing scope, and a
  missing or invalid CSRF token are refused before any audit write. They write
  no `call.hangup` row.
- A successful hangup writes one `call.hangup` row with outcome `success`. Only an
  administrator ending another user's call records that user as the target.

What happens depends on the call's state. A queued call is failed without a
dial. A dial in flight is ended when it returns. A live call's leg is hung up and
its AI session is closed, and the call ends on Telnyx's `call.hangup`. Each
successful or refused request writes one `call.hangup` audit row.

### Ring timeout

`CALLHARBOR_RING_TIMEOUT_SECONDS` is sent to Telnyx as the dial's timeout, so a
callee who does not answer releases the leg after that many seconds. The call then
ends through the normal hangup path.

### Restart reconciliation

At startup, before the first dial, the server reconciles calls left by the
previous process. A call left in `initiating`, or in `ending` after a hangup
during its dial, with no provider id is failed with `dial_unconfirmed`, because
its dial may have reached Telnyx without an id being recorded. Failing it frees
its concurrency slot. A call with
a provider id that was live (`initiating`, `ringing`, `active` or `ending`) has no
media session in this process, so it is ended, with one hangup for its leg. Queued
calls are dialed once, after reconciliation.

Recovery is a single startup pass. It is bounded by the global concurrency limit
and by a two-minute budget for all of its hangups. Each hangup also has a 30-second
client timeout. A hangup past the budget still ends the database leg, and its
Telnyx hangup is left to the sweep below.

A sweep then runs on each worker tick (the worker wakes every 5 seconds and on
demand) and handles calls the startup pass could not settle:

| Constant | Value | Meaning |
| --- | --- | --- |
| Retry interval | 60 s | Minimum time between hangup attempts on one call. |
| Grace | 60 s | Added to each deadline before a call is treated as overdue. |
| Max attempts | 5 | Hangup attempts before the sweep gives up on the leg. |
| Restart budget | 2 min | Time limit for the startup hangups as a whole. |

An `ending` call is checked against Telnyx's `GET /v2/calls/{id}`. If Telnyx
reports the leg not alive, or answers 404 or 422, the call ends and keeps the
reason it already had, such as `hangup_requested`. Only the startup pass writes
`restart_reconciled` for a call it ends. If Telnyx reports the leg alive, the same
hangup command is resent. A ringing or active call past its time limit plus ring
timeout and grace ends with `max_duration`. An initiating call past the ring timeout
and grace ends with `reconcile_timeout`. A call that has used its max attempts and
is also past its expiry deadline ends with `reconcile_expired`. Until then the
hangup is still resent, so a leg is never given up while it may still be live.

These constants are fixed in code. They are not configuration.

Every deadline is measured from the call's `created_at`, not from when the call
started or was last updated. A leg's expiry deadline is `created_at` plus the
Telnyx time limit (the call's cap clamped to 30 to 14400 seconds), plus the ring
timeout and the grace. This is the cost-safe choice: a leg is never given up
before Telnyx could have ended it, however long the call waited to start.

The orphan sweep covers only terminal calls whose expiry deadline has not passed.
A terminal call from before the hangup markers existed, whose deadline is past,
gets no hangup, because Telnyx has already ended its leg. A call still inside its
window is hung up as before.

### Cost

A call's cost is shown in `details.cost`:

| `status` | Meaning | `source` |
| --- | --- | --- |
| `known` | Telnyx reported the charge in a signed `call.cost` event. | `provider` |
| `estimated` | No provider charge yet. The operator's rate times the call's duration. | `estimate` |
| `unavailable` | Neither exists. `amount`, `currency` and `source` are null. | null |

`amount` is a decimal string, never a float. A known cost wins over an estimate.
A provider error report does not make the cost known, so the estimate still
shows. A `call.cost` with parts in more than one currency is stored as having no
amount, so the cost is `unavailable` rather than a guessed sum.

A `call.cost` event is recorded whenever it binds to the call, in any state. Cost is
not a state transition, so a cost that arrives before `call.hangup` (reordered
delivery) is stored and acknowledged with `200`, and the call's state does not
change. The hangup then ends the call as usual. A cost for a call that was never
dialed is refused permanently with `200` and stores nothing, as a lifecycle event
for that call is.
A cost whose control id belongs to another leg is refused and not stored. A
second, different cost for a call that already has one is refused too, so the
first stands. Both refusals are permanent: Telnyx gets a 200 and does not retry.
A redelivery of the same event writes nothing new.

To receive provider costs, enable `call_cost_in_webhooks` on the Telnyx Call
Control application. Without it, CallHarbor never receives `call.cost`, and
calls show their estimate or `unavailable`.

Estimates come only from `CALLHARBOR_CALL_COST_ESTIMATE_PER_MINUTE`. CallHarbor
has no built-in tariff, so the estimate is only as accurate as the rate the
operator sets. An estimate is never presented as a provider charge.

## Rollback past migration 0007

Migration `0007_call_telephony_grants` lets a grantee's call reference the
owner's telephony profile. Rolling back past 0007 restores the original
owner-only foreign key.

**Before rolling back past 0007, purge every call owned by a grantee that uses a
profile granted to someone else.** The down migration fails with a foreign key
violation while any such call remains.

Today the only purge is the storage function `PurgeCall`, which deletes one call
and its retained data. No operator command or HTTP route exposes it yet, so
removing these calls is not a supported operator step. Until a supported purge
exists, do not roll back past 0007 on an instance that has placed grantee calls.

## Rollback of migration 0009

Migration `0009_call_safety` adds the per-user call policy, the cost columns, and
the hangup markers. Rolling it back, to 0008, drops two things permanently:

- **Per-user policies.** The `user_call_policies` table is dropped, so every
  per-user limit and allowance is lost. Installation policy from the environment
  still applies after a rollback, but the per-user values cannot be recovered.
- **Cost data.** The estimate and provider cost columns are dropped, so recorded
  costs are lost. Calls are kept.

The hangup-marker columns are also dropped, so the restart and sweep state for
in-flight hangups is lost. Roll back only on an instance with no live calls.

## What is not yet verified

Be clear about these when relying on calls in production:

- **Ringing is inferred.** The server marks a call `ringing` from Telnyx's
  `call.initiated` event. This is not yet checked against live Telnyx behavior.
- **Instruction ordering is proven against fakes only.** The tests show the
  instructions reach the AI provider before any caller audio. Real providers have
  not been tested for this ordering.
- **`record_effective` is always false until issue #70.** A requested recording
  is stored as `record_requested` only. Nothing records audio yet.
- **Local hangup is verified against fakes only.** The hangup request, its
  command id, and the `ending` to `ended` path are tested against the fake
  Telnyx. Whether live Telnyx ends the leg as expected is unverified.
- **Call status lookup is unverified.** Restart and sweep reconciliation read
  `GET /v2/calls/{id}`. The server treats a 404 or 422 as "leg not alive". That
  reading is not checked against live Telnyx, and a wrong reading could end a live
  leg early or keep a dead one in `ending`.
- **`call.cost` delivery is unverified.** Provider costs need
  `call_cost_in_webhooks` enabled on the Call Control application. Until a live
  call confirms the event arrives and carries the fields CallHarbor reads, costs
  show as estimated or unavailable.
- **Ring timeout and the dial timeout are unverified live.** The configured value
  is sent to Telnyx as the dial's timeout. Whether Telnyx releases the leg at
  exactly that point has not been observed.
- **No live Telnyx or AI calls have been made.** All automated tests use
  in-process fakes. Behavior against real Telnyx and real AI providers is
  unverified.
