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

The call worker runs only when `CALLHARBOR_PUBLIC_URL` is set **and** the master
key is present. Without either, calls are disabled and the server still starts,
so OAuth and MCP keep working. `POST /api/v1/calls` and `make_call` then return
`503 unavailable` with reason `calls_unavailable`, and no call row is written.
At startup the log says `calls are disabled until CALLHARBOR_PUBLIC_URL is set`
or `calls are disabled: they need the secrets master key`.

## Telnyx URLs

The server derives these from `CALLHARBOR_PUBLIC_URL`. Configure nothing else in
Telnyx beyond the connection and its webhook URL.

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
`time_limit_secs` is the backstop. Fuller reconciliation is tracked for #65.

The container's stop grace period must cover the worst-case shutdown sequence.
With `T` set to `CALLHARBOR_SHUTDOWN_TIMEOUT` and `B = max(T/3, 5 s)`, the sum
is `T + 5 s + 3B`: 35 s at the default 15 s, and 605 s at the 5 m maximum. At
the default, use `docker stop --time 40` or Compose `stop_grace_period: 40s`.
See `docs/operations/docker.md`.

Dials are serial: one worker places one call at a time, so a slow dial delays
the dials queued behind it. Head-of-line blocking is tracked for issue #65.

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
- **No live Telnyx or AI calls have been made.** All automated tests use
  in-process fakes. Behavior against real Telnyx and real AI providers is
  unverified.
