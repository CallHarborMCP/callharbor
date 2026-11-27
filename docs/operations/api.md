# REST API v1

CallHarbor serves a versioned REST API under `/api/v1`. The machine-readable contract is `api/openapi.json` (OpenAPI 3.1). The tests in `internal/api` fail if the document and the mounted routes disagree.

## Authentication

Every route requires a signed-in browser session cookie, `callharbor_session`. Identity comes only from that session. No route accepts a user or owner identifier, and a request cannot choose another user's data. Anonymous requests receive `401 unauthenticated`.

MCP and OAuth tokens are not accepted on `/api/v1`. The MCP endpoint has its own contract; see `oauth-mcp.md`.

## Routes

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/v1/profiles` | The caller's AI and telephony profiles, without secrets |
| `GET` | `/api/v1/calls` | The caller's calls, newest first, one page at a time |
| `POST` | `/api/v1/calls` | Queue one outbound call; returns `202` (requires `Idempotency-Key`). See [Placing calls](#placing-calls) |
| `GET` | `/api/v1/calls/{call_id}` | One call owned by the caller |
| `POST` | `/api/v1/calls/{call_id}/hangup` | End a call the caller owns, or any call for an administrator; returns `200` with the current state (see [Ending calls](#ending-calls)) |
| `GET` | `/api/v1/call-policy` | The caller's effective call policy (see [Call policy](#call-policy)) |
| `GET` | `/api/v1/users/{user_id}/call-policy` | A user's stored overrides and effective policy (admin) |
| `PUT` | `/api/v1/users/{user_id}/call-policy` | Set a user's overrides, which can only tighten the installation policy (admin) |
| `GET` | `/api/v1/telephony-profiles` | Telephony profiles the caller owns or was granted |
| `POST` | `/api/v1/telephony-profiles` | Create a telephony profile (owner) |
| `GET` | `/api/v1/telephony-profiles/{profile_id}` | One usable telephony profile |
| `PUT` | `/api/v1/telephony-profiles/{profile_id}` | Replace a profile's fields and secrets (owner) |
| `DELETE` | `/api/v1/telephony-profiles/{profile_id}` | Delete a profile not referenced by calls (owner) |
| `POST` | `/api/v1/telephony-profiles/{profile_id}/validate` | Read-only connectivity check (owner) |
| `GET` | `/api/v1/telephony-profiles/{profile_id}/grants` | List grants on a profile (admin owner) |
| `PUT` | `/api/v1/telephony-profiles/{profile_id}/grants/{user_id}` | Grant a user use of a profile (admin owner) |
| `DELETE` | `/api/v1/telephony-profiles/{profile_id}/grants/{user_id}` | Revoke a grant (admin owner) |

Only these operations are implemented. Other methods on these paths are not routed. Planned operations (recordings, tracked in later issues) are not exposed by this API.

Planned operations are listed in `api/openapi.json` with `x-callharbor-availability: planned`. They are never mounted as operations. Their responses are never a success and never start anything:

- A planned `GET` path (the recording and recording audio paths) is answered by the API itself. A request needs a valid session, as every `/api/v1` path does. Anonymous requests get `401 unauthenticated`. A signed-in request gets `404` with the `not_found` envelope, `{"error":{"code":"not_found",...}}`. It never reaches the web UI, so the answer is the same whether or not the UI is built.
- A planned `POST` path returns `405 Method Not Allowed` from the router, plain text and not the JSON envelope. Because the web UI's `GET /` pattern matches every path, the router reports a wrong method as 405 rather than 404. For example, `POST /api/v1/calls/{call_id}/transcript` returns `405`, because that path is mounted for `GET` only.

The planned `GET` list is derived from `PlannedRoutes` in `internal/api/v1.go`, and the app tests probe every planned route with a session.

## Call transcripts

`GET /api/v1/calls/{call_id}/transcript` returns one page of a call's transcript. It is also available as the MCP tool `get_transcript`, with the same fields.

- **Ownership.** Only the call's owner can read it. A call owned by another user, or a missing call, returns `404 not_found` with the same message, and no turn text.
- **Pages.** A page holds up to 100 turns in `seq` order. Pass `next_cursor` back as `?cursor=` for the next page. `next_cursor` is an opaque string, empty on the last page. A cursor longer than 512 bytes, or one that does not decode, returns `400 invalid_input`.
- **Turns.** Each turn is the latest revision of one `seq`. A later `revision` replaces an earlier one and never edits it. `speaker` is `assistant`, `callee` or `system`. `finality` is `partial` or `final`. `started_ms` and `ended_ms` are millisecond offsets on the timeline named by `timing_source`. `ended_ms` is `null` when unknown.
- **Provenance.** `fidelity` says how far the text matches what was heard and played: `played`, `interrupted_text_includes_unplayed` (the text includes audio that was generated but cut off before it played), `playout_unconfirmed`, `not_played`, `provider_asr_approximate` (approximate text from the provider's recognition or, on `realtime_ws`, from the voice model's out-of-band transcription, flagged `caller_text_model_derived`; not an exact record) or `unavailable`. `timing_source` is `provider` (the voice provider's session timeline) or `local_receive` (when CallHarbor received the event). `generated_ms` and `played_ms` are `null` when unknown. `played_is_estimate` is `true` when `played_ms` is inferred rather than confirmed. Provider identifiers appear as `correlation_id`, `provider_item_id` and `source_ids`.
- **Page flags.** `quality_flags` on the page merges the call's stored flags with two derived ones: `transcript_in_progress` while the media run is open, and `no_media_session` when the call never reached a media run. `finalized` is `true` once the run has ended. The flags are open-ended strings, so clients must accept values they do not know. The documented vocabulary is `caller_transcript_unavailable`, `transcript_in_progress`, `transcript_not_finalized`, `no_media_session`, `two_party_incomplete`, `agent_text_includes_unplayed`, `agent_text_not_played`, `playout_unconfirmed`, `playout_attribution_local`, `playout_updated`, `events_dropped`, `text_truncated`, `turns_grouped_locally`, `timing_local_receive`, `provider_turn_incomplete`, `provider_id_invalid`, `out_of_order_fragments`, `agent_transcript_missing`, `unexpected_caller_transcription_ignored`, `corrected`, `caller_transcript_pending`, `caller_text_model_derived`, `caller_transcript_failed`, `caller_transcript_unresolved`, `caller_text_empty`. Caller flags for `realtime_ws` out-of-band text: `caller_transcript_pending` marks a caller turn whose transcription has not resolved yet and is never shown as complete. `caller_text_model_derived` marks caller text produced by the configured voice model, which is approximate. `caller_transcript_failed` marks a caller turn whose transcription failed, was rejected, timed out or was cancelled. `caller_transcript_unresolved` marks a caller turn still pending when the call ended. `caller_text_empty` marks a successful transcription with no text. The `caller_transcript_unavailable` flag is no longer set for `realtime_ws`. Meanings: `playout_attribution_local` marks every Live agent turn. Live agent audio has no item ID, so its utterance grouping uses the local caller-speech edges, and playout is attributed to it locally. Live agent text is not split on the provider timeline; that split is not done. `playout_updated` means playout evidence changed after the final text, without the text changing. `provider_id_invalid` means an oversized provider identifier was dropped, not stored, and not remembered for replay. `transcript_not_finalized` means the call ended without a finalized transcript summary. `corrected` means the final text changed after a final revision had been written.
- **Provider events.** The call timeline keeps each provider event as its type, provider event, item and response IDs, correlation, and safe scalar diagnostics (for example status, usage and error code), per `.agents/ARCHITECTURE.md` ("retain raw provider IDs and diagnostic metadata separately"). Verbatim raw provider payloads are deliberately not stored: transcript text lives in the transcript turns and audio lives in the recording. This is a stated limitation, not raw-event storage. Transcript text is stored in SQLite without app-level encryption; this is inherited, and secrets use AES-GCM via `internal/secrets`.
- **Two-party transcript.** On `realtime_ws`, the caller's (`callee`) text is requested through an out-of-band response from the same configured voice model for each caller audio item. It is model-derived and approximate, with local receive timing, and it is flagged `caller_text_model_derived` and `timing_local_receive`. It is not a dedicated ASR or transcription service, and it is never labelled verbatim. This path is verified only against CallHarbor's independent emulators. Whether the real endpoint accepts these responses, and what extra quota they use, is unverified and needs an owner-run billable check (open risk R18 in `docs/protocol/FEASIBILITY.md`).

Failure handling:
- **Rejected.** The endpoint answers with an `error` that names the request's event id. The caller turn is `caller_transcript_failed` with `out_of_band_rejected:<code>`. An `invalid_request_error` also marks the endpoint unsupported, so later caller items fail `out_of_band_unsupported` with no request sent. A rate limit or server error fails only its item when it names the request's event id. An error that names a CallHarbor out-of-band event id (`ch_oob_` requests or `ch_oob_cancel_` cancels) is never a call error, even when its request has already finished; it is dropped.
- **Ignored.** The response carries the purpose echo but a non-null `conversation_id`, so the endpoint did not honour `conversation: "none"`. The item fails `conversation_not_none` and the endpoint is unsupported. Audio from the response fails the item `modalities_ignored`, and CallHarbor cancels that response only.
- **Unresolved.** A request with no answer after 30 s fails `timeout` when the next server message is read, or becomes `caller_transcript_unresolved` at close. A caller turn still pending at close is `caller_transcript_unresolved`.
- **Not attributable.** An `error` with no `error.event_id` is attributed to the only pending request only when no other request is pending, that request has no response id yet, and `error.param` is empty or starts with `response`. Any other such error is an ordinary call error. A heuristic-attributed error fails only its item and never latches the endpoint, whatever its type.

Every failure keeps the transcript incomplete (`two_party_incomplete`). A caller turn with partial text and no final is not complete, even when it has text.

**Empty output.** A response that completes with no speech and no text is a successful empty transcription (`caller_text_empty`). This does not make the transcript incomplete by itself. `recording.Complete` accepts an empty caller entry, and the prompt asks for no output when there is no speech. A response with non-text output and no text part is `missing_text`, a failure.

**Timing bound.** Writes made for out-of-band requests and cancels use a 5 s deadline, detached from the read context so a frame is never cut mid-write. The timeout check runs when a server message is read; a silent socket is not woken by a timer.

Do not treat a transcript from this path as a complete record of both parties unless the page carries no `two_party_incomplete` flag.
- **Audit.** Every read that returns a page, and every refused read of a foreign or missing call, writes a `transcript.read` event to the audit log. The event records the actor and the call. A read whose audit write fails returns `500 internal` and no transcript.

## Telephony profiles

Every telephony route needs a valid session. Each write also needs the session's CSRF token in the `X-CSRF-Token` header; a missing or wrong token returns `403 forbidden`.

- **Secrets are write-only.** `api_key` and the optional `webhook_public_key` are accepted on create and replace. No response, log line or error contains them. On replace, an omitted `api_key` keeps the stored key. On replace, an omitted or empty `webhook_public_key` keeps the stored key, and JSON `null` removes it. `null` is refused on create.
- **Connection application.** `connection_id` is required on create and replace, at most 128 bytes. Calls dial through this Call Control application, so a profile without one cannot place a call. A profile saved before this field became required has no `connection_id`, and its validation fails with `application_not_found` until it is replaced. The owner sees `connection_id`. A granted user does not.
- **Request bodies are one JSON value.** Anything after the value, including a second value or a stray bracket, returns `400 invalid_input`.
- **Access.** A profile is visible to its owner and to users it was granted to. Anyone else gets `404`, so the existence of another user's profile is not revealed.
- **Grants are admin-only.** Only an admin who owns the profile may list, grant or revoke. A non-admin gets `403` on every grant route, including the owner's.
- **Validation** checks the stored key, the Call Control application named by `connection_id` (always checked, since `connection_id` is required) and the caller number. It sends GET requests only and never places a call. The result is stored as `connectivity_status` (`verified` or `failed`) with a fixed `connectivity_reason` code. The reason codes are `invalid_credentials`, `application_not_found`, `application_inactive`, `number_not_found`, `number_inactive`, `number_not_on_application`, `provider_unreachable` and `secrets_unavailable`. Provider text is never returned.
- **Changes reset validation.** Updating any field sets `connectivity_status` back to `unknown`. A validation that finishes after the profile was changed is refused with `409 conflict` (`telephony profile changed during validation`) and does not overwrite the stored result. The audit log records the attempt as `failed` with the detail `changed_during_validation`. Validate again.
- **Deletes** are refused with `409 conflict` while calls reference the profile.
- **Secrets unavailable.** When the master key is not configured, a create or replace that needs to store a secret returns `503` with the envelope code `internal`, because the shared code set has no unavailable code. The OpenAPI document lists this `503` response on both operations. Nothing is persisted on that path.
- **Update schema.** `PUT` uses `TelephonyProfileUpdate`, in which `api_key` is optional. `POST` uses `TelephonyProfileWrite`, in which `api_key` is required.

## Listing calls

`GET /api/v1/calls` uses keyset pagination over `(created_at DESC, id DESC)`.

- `limit`: integer from 1 to 100. Defaults to 50. `0`, values above 100, and non-integers return `invalid_input`.
- `cursor`: the opaque `next_cursor` from the previous page. A missing or empty cursor means the first page. A malformed or tampered cursor returns `invalid_input`. A cursor longer than 512 bytes returns `invalid_input` and is never decoded or sent to the database. An empty `next_cursor` means the last page.
- A cursor timestamp with a non-UTC offset, such as `+02:00`, names the same instant and is normalized to UTC. Sub-millisecond digits are truncated to milliseconds.
- `status`: optional. One of `queued`, `initiating`, `ringing`, `active`, `ending`, `ended`, `failed`. Any other value returns `invalid_input`.

Cursors are opaque and should not be parsed or constructed by clients.

## Task outcome

Every call has two separate facts. Neither implies the other.

- `technical_status` says what the telephony and media path did: `queued`,
  `initiating`, `ringing`, `active`, `ending`, `ended` or `failed`. A successful
  dial or a normal hangup reaches `ended` without any claim about the task.
- `task_outcome` says whether the task was achieved: `success`, `partial`,
  `failed` or `unknown`. It is `null` until the outcome is evaluated.

A normal hangup is never `success`. A call that ends `ended` with no evidence of
the task being done is `unknown`, not `success`.

`details.task_assessment` gives the reasoning behind `task_outcome`. It is `null`
until an outcome is recorded, and then it is an object:

- `reason_codes`: ordered, de-duplicated codes (below).
- `evidence`: up to 32 references. Each has `kind` (`transcript_turn` or
  `call_event`), `seq`, and where relevant `criterion` (1-based into the brief's
  success criteria), `met`, `quote`, `speaker`, `fidelity` or `code`. For
  `call_event` evidence `seq` is the call event's own sequence number
  (`call_events.seq`). It is separate from the transcript turn `seq`, and it is
  not exposed as a public read today. `met` is set only on transcript evidence
  that names a criterion. It is omitted for `call_event` evidence and for
  outcome-level quotes.
- `source`: `technical` when no voice-session self-report was used, or
  `voice_session` when the outcome rests on one.
- `assessed_at`: the RFC 3339 UTC time the outcome was recorded. Recording the
  outcome does not change the call's `updated_at`; watch `task_outcome` or
  `task_assessment.assessed_at` to see that it has been recorded.

The same object appears on `GET /api/v1/calls/{call_id}`, on list items, and on
the MCP tools `get_call` and `list_calls`.

### Reason codes

`reason_codes` mixes two vocabularies. The call's technical reason is unchanged
and stays in `details.outcome_reason_code`.

Technical codes name what happened on the call:
`no_answer`, `busy`, `rejected`, `call_failed`, `media_failed`,
`transcript_unavailable`, `transcript_incomplete`, `no_conversation`.

Task codes name why the outcome is what it is:

| Code | Meaning |
|---|---|
| `confirmed` | The callee confirmed the requested outcome. |
| `alternative_accepted` | The callee accepted an alternative within the brief's constraints. |
| `partially_fulfilled` | Some criteria were met, not all. |
| `callee_refused` | The callee refused. |
| `callee_unable` | The callee could not help. |
| `voicemail` | The call reached voicemail. |
| `wrong_party` | The callee was not the intended party. |
| `other` | None of the above. |
| `assessment_missing` | No voice-session report was made. |
| `assessment_unsupported` | The protocol cannot report an assessment (`live_ws`). |
| `assessment_invalid` | A stored report failed validation and was skipped. |
| `assessment_contradictory` | The report's outcome and reason disagree. For example, a failed or partial outcome with a success reason (`confirmed`, `alternative_accepted`). The result is `unknown`. |
| `assessment_conflicting` | Valid reports for the same call name different outcomes. The result is `unknown`. The latest report does not win. |
| `evidence_unverified` | A quote did not match a final callee turn, was too weak (fewer than 3 words and 12 letters), or was preceded by a negation in the callee's turn. |
| `evidence_missing` | A required criterion has no verified evidence. |
| `evidence_truncated` | Evidence was cut to the 32-item bound. |
| `criteria_unmet` | A success claim has some criteria met but not all. |
| `constraint_violated` | A brief constraint was reported as not respected. |
| `constraint_unreported` | A success omitted a brief constraint. An omitted constraint is not treated as respected. |
| `evaluation_bound_exceeded` | The call's transcript is larger than the evaluation bound. The result is `unknown`, so the call is not retried forever. |
| `evaluation_input_invalid` | The call's stored brief or facts cannot be read. The result is `unknown`, so the call is not retried forever. |

### Evidence semantics

- A quote matches a transcript turn by whole words, after lowercasing, removing
  apostrophes, and turning every other non-letter, non-digit character into a
  space. A match must start
  and end on a word boundary, so `no` never matches inside `not`. This is not a
  substring match. Quotes longer than 500 characters are unverified.
- A quote needs at least 3 words or 12 letters after normalization. A shorter
  quote such as `yes` is unverified, so it can never make an outcome a success.
- Negation applies only to a met quote. A met quote is unverified when:
  - a negation word appears within the two words before it in the same callee
    turn. The negation words are `not`, `never`, `cannot`, `can't`, `won't`,
    `don't`, `isn't`, `unable`, `nobody`, `nothing`, `neither`, `nor` and
    `without`. So `we cannot book a table for two` does not verify `book a table
    for two`;
  - an n't contraction (see below), such as `didn't`, `couldn't`, `wasn't`,
    `wouldn't` or `haven't`, appears in that window;
  - the quote itself is negative: it contains a negation word or contraction, or
    `no` followed by a word with no comma or sentence punctuation between them.
    So `could not book a table for two` and `no table for two at eight` never
    verify a met criterion.
- An apostrophe joins the letters on either side into one word, so `can't` is
  `cant`. The apostrophe characters are `'`, `’` (U+2019), `‘` (U+2018), `ʼ`
  (U+02BC), `＇` (U+FF07), the prime `′` (U+2032) and the backtick. A word is
  joined only when a letter follows the apostrophe, so a quoted word such as
  `'agent'` is not negated. A joined word is an n't contraction only when the
  letters after its last apostrophe are exactly `t` and the word ends in `nt`:
  `didn't`, `couldn't` and `haven't` negate, but `l'instant` and `O'Grant` do
  not.
- `no` negates only when it is the word immediately before the quote, with no
  comma or sentence punctuation between them. So `No problem, booked a table for
  two` verifies, and `No table for two` does not.
- A matched span must not cross a sentence end. `!` and `?` always end a
  sentence. A `.` ends one only when it is followed by whitespace and then an
  uppercase letter, or by the end of the text, and the word before it is not a
  single letter, all digits, or one of `mr`, `mrs`, `ms`, `dr`, `st`, `jr`, `sr`,
  `vs`. A `.` between two digits is part of a number. So `8 p.m. tonight`, `8.30`,
  `$12.50` and `Dr. Smith` do not end a sentence, but `We cannot. Book a table`
  and `Lunch is free. Book a table` do.
- The two-word window is a known over-block that fails safe. A negation word
  within two words before the quote is treated as negating it, even when the
  word is not part of a negation. So `don't worry, book a table for two` and
  `never mind, book a table for two` do not verify a met criterion. A real
  confirmation can be marked unverified this way. It is not a false success.
- A quote for an unmet criterion, such as a refusal, is never checked for
  negation. Outcome-level quotes are not checked for negation under a failed
  outcome, because any verified quote supports failure.
- A quote resolves to the earliest final callee turn that fully supports it, by
  the same rule used to verify it. Only final callee turns can support evidence.
  Caller turns and partial turns never do. An unmatched quote stays unverified.
- A final callee turn makes a call answered only when the call's effective reason
  is not `no_answer` or `busy`, or an answered event exists. Carrier early media
  can produce a callee turn for a call that never connected.
- Callee turns are the words the other party spoke. On `realtime_ws` the
  transcript of those words is model-derived and approximate. Verification
  proves provenance only: the quoted words appear in the stored transcript of
  the callee. It does not prove the meaning or the truth of what was said. A
  callee can also say words that instruct the model (prompt injection). Such
  words are still only text. They can be quoted only if they appear in the
  transcript, and they cannot make an outcome a success without the rules above
  holding.
- Verified evidence is kept on `unknown` results too, so a reader can see what
  was checked. This is an accepted deviation from a stricter reading of the
  design, and the result stays `unknown`.
- A quote for a criterion the session reported not met (`met: false`) is kept as
  evidence, for example a refusal. It never counts toward success or partial.
- Evidence is bounded to 32 items. Excess items add `evidence_truncated`.

### Deciding the outcome

- `success` needs a success reason, every brief constraint reported and
  respected, and every criterion reported met and verified. A brief with no
  criteria needs at least one verified met quote. Quotes may be given at the
  outcome level (`evidence_quotes`) for this.
- `partial` needs at least one verified, met criterion. A partial claim with a
  success reason is `assessment_contradictory`.
- `failed` needs at least one verified quote, on any criterion or at the outcome
  level, such as a refusal or a voicemail greeting. A failed claim with a
  success reason is `assessment_contradictory`.
- An omitted brief constraint in a success is `constraint_unreported`.
- Valid reports that disagree on the outcome are `assessment_conflicting`.

### Voice-session assessment

The outcome is assessed only from the voice session that handled the call. No
other model is used.

- On `realtime_ws` calls the model is offered one tool, `report_task_outcome`.
  It reports `success`, `partial` or `failed`, a closed-set reason, a met or not
  met flag per criterion with the callee's exact words, and a respected or not
  respected flag per constraint. CallHarbor acknowledges each valid report and
  never asks the model to speak in reply.
- On `live_ws` calls no assessment is collected, so the result carries
  `assessment_unsupported` and is `unknown` unless the call was never answered.
- A report that fails validation is stored only as an invalid marker, never its
  raw arguments. It adds `assessment_invalid`. CallHarbor acknowledges an invalid
  report that has a usable call id with the function output
  `{"status":"invalid"}`, so the model's call does not wait. A report with no
  usable call id has nothing to answer and gets no acknowledgement. A usable
  report is acknowledged with `{"status":"recorded"}`.
- The report's criteria and constraints are bounded to 20 items each, the same
  limit as the brief's success criteria and constraints.
- Evidence cites the callee's words only. The stored caller transcript on
  `realtime_ws` calls is model-derived and approximate, so it is never quoted as
  evidence.
- Whether the real Realtime endpoint accepts this tool and calls it reliably is
  **unverified** (`docs/protocol/FEASIBILITY.md`, R19). Until that is checked, a
  missing report yields `unknown` with `assessment_missing`, and a failed or
  invalid report yields `unknown` with `assessment_invalid`.

### Timing

A terminal call waits 2 minutes after it ended before its outcome is evaluated,
whether or not it was answered. A call that hung up before its answered signal
arrived can still be completed by a late answered signal within that time. A
transcript that is still unfinished at that point is reported as
`transcript_incomplete`. The outcome is recorded once and is not changed later.

## Errors

Every error uses one envelope:

```json
{"error": {"code": "not_found", "message": "call not found"}}
```

| Code | HTTP status | Meaning |
| --- | --- | --- |
| `unauthenticated` | 401 | No valid session |
| `forbidden` | 403 | Authenticated but not allowed |
| `insufficient_scope` | 403 | Token scope is too narrow (not used by session routes) |
| `not_found` | 404 | Missing, or owned by another user. The two cases are indistinguishable. |
| `invalid_input` | 400 | Bad query parameter or path value |
| `conflict` | 409 | State conflict, including `idempotency_conflict` |
| `unavailable` | 503 | Calls are not running (see `calls_unavailable` under [Placing calls](#placing-calls)) |
| `internal` | 500 | Unexpected failure. The message is always `internal error`; details go to the server log only. |

## MCP tool errors

The MCP tools use the same codes and the same `{"error":{"code","message"}}` text as this API. There are two kinds of failure:

- **Service errors** come from the shared service layer: `not_found`, `invalid_input`, `insufficient_scope`, `unauthenticated` and `internal`. The tool result has `isError` set and no structured content, and its text is the envelope.
- **Schema rejections** come from the go-sdk before the handler runs. An unknown `status`, a `limit` outside 1 to 100, or a `cursor` over 512 bytes is refused against the tool's JSON schema. The result has `isError` set, but its text names the rule that failed and is not the service envelope. Clients should check `isError` first, and read the envelope only when the text parses as one.

`internal` always carries the message `internal error`. The cause is logged on the server only.

## Idempotency

Commands that create work take an `Idempotency-Key` header. Today that is `POST /api/v1/calls` and the `make_call` MCP tool. The key is 1 to 128 characters, must be valid UTF-8, and may not contain whitespace or non-printable characters. An invalid key is rejected with `invalid_input` before any work starts. The format check is `service.ValidateIdempotencyKey`.

Keys are scoped to the owner. A repeated request with the same key and the same request returns the current call and its original `created_at`, and does not dial again. The same key with a different request is `409 idempotency_conflict`. The comparison covers the telephony profile, the number, `record`, `max_duration_seconds` and the canonical brief. It does not cover the AI profile, which the server selects from the caller's own profiles.

## Placing calls

`POST /api/v1/calls` takes a JSON body with `profile_id`, `to` (E.164), `objective`, and optional `recipient`, `language`, `context`, `instructions`, `constraints`, `fallbacks`, `success_criteria`, `max_duration_seconds` (1 to 7200, default 600) and `record`. Unknown fields are rejected. It needs a session cookie and the CSRF token. The body is capped at 300 KiB.

It returns `202` with `{"call_id", "status": "queued", "created_at"}`. The dial happens later, in the call worker, so a `queued` status is the only status the response can show. Poll `GET /api/v1/calls/{call_id}` for progress.

Refusals and their codes:

| Code | When |
| --- | --- |
| `invalid_input` (400) | Malformed JSON, a missing `Idempotency-Key`, or a field out of range |
| `brief_too_large` (400) | The rendered brief is over `CALLHARBOR_MAX_CALL_BRIEF_BYTES`. The brief is never truncated. The message names the setting. |
| `ai_profile_required` (400) | The caller owns no AI profile |
| `ai_profile_ambiguous` (400) | The caller owns more than one AI profile. No request field selects one. |
| `not_found` (404) | Unknown or ungranted telephony profile. The response is the same in both cases. |
| `telephony_not_verified`, `telephony_connection_missing`, `telephony_webhook_key_missing` (400) | The telephony profile is not ready. Validate it first. |
| `unavailable` (503), reason `calls_unavailable` | The call worker is not running (see below). No row is written. |
| `forbidden` (403), reason `destination_denied`, `destination_not_allowed` or `cost_limit_exceeded` | The destination is denied or not in the allow list, or the estimate is over the per-call cost limit. No row is written. |
| `invalid_input` (400), reason `max_duration_exceeds_policy` | An explicit `max_duration_seconds` is above the effective policy maximum. No row is written. |
| `conflict` (409), reason `cost_unavailable`, `concurrency_limit` or `global_concurrency_limit` | A cost limit has no estimate rate, or a capacity limit is reached. No row is written. |
| `rate_limited` (429), reason `rate_limited` | The user has placed the maximum calls for the hour. No row is written. |

Each refusal message starts with its reason code, then a colon and a short
explanation. The full list, and the setting behind each, is in the
[calls and Telnyx setup guide](calls-telnyx.md#refusals).

`calls_unavailable` means the server is missing `CALLHARBOR_PUBLIC_URL` or the master key. The worker runs only when both are set, so the server refuses calls instead of queuing them. At startup the log says `calls are disabled until CALLHARBOR_PUBLIC_URL is set` (info) or `calls are disabled: they need the secrets master key` (warning). See the [calls and Telnyx setup guide](calls-telnyx.md).

## Ending calls

`POST /api/v1/calls/{call_id}/hangup` ends a call. It takes no body and needs the
session cookie and the CSRF token. The owner may end their own call, and an
administrator may end any call through this REST route. Any other caller gets
`404 not_found`, and that refusal is audited with outcome `denied` and no owner in
it. A missing CSRF token gets `403 forbidden` and changes nothing.

The response is `200` with `{"call_id", "technical_status", "task_outcome"}`. It
is the state the request leaves the call in. A live call reports `ending` until
Telnyx confirms the hangup. A call that has not been dialed is ended without a
dial. A repeat request returns the current state. For a call still `ending`, it
may send the hangup again, but every send carries the same command id, so Telnyx
receives one distinct command. The MCP tool `hangup_call` does the same for the caller's own calls and
needs the `mcp:call` scope. MCP actors are never administrators.

Only a successful hangup and a cross-owner refusal write a `call.hangup` audit row.
A `404` for a missing call, an invalid `call_id`, a missing scope, and a CSRF
refusal write none. See the [calls guide](calls-telnyx.md#emergency-hangup).

## Call policy

Each user's effective call policy combines the installation settings with an
administrator's per-user overrides. Overrides can only tighten a limit: the
effective value is the stricter one. `GET /api/v1/call-policy` returns the caller's
own effective policy. The admin routes return both the stored overrides and the
effective result for one user. A write is refused with `invalid_input` when a value
is out of range, and with `forbidden` for a non-admin. Money is a decimal string,
never a float. The installation settings are read at startup, so a change to them
takes effect after a restart. See the [calls guide](calls-telnyx.md#per-user-policy)
for the setting names.

## Response conventions

- Timestamps are UTC RFC 3339 with a `Z` suffix, for example `2026-11-25T10:00:00.000Z`.
- A call's `details.cost` is `{"status", "amount", "currency", "source"}`. `status` is `known` (the provider's reported charge), `estimated` (the operator's rate times the duration) or `unavailable`. `amount` is a decimal string, never a float, and `currency` is an ISO 4217 code. `source` is `provider` or `estimate`. For `unavailable`, all three value fields are `null`. A known cost wins over an estimate. See the [calls guide](calls-telnyx.md#cost).
- `get_profiles` (REST `GET /api/v1/profiles` and the MCP tool of the same name) lists telephony profiles the caller owns **or was granted**. A granted profile appears with `access: "granted"`, and it is listed only while its owner is an active admin and the caller is active. Demoting or deactivating the owner hides it from the grantee until the owner is restored.
- `connectivity_status` on telephony profiles is the last read-only validation result: `"unknown"` until a validation runs (and again after any change to the profile), then `"verified"` or `"failed"`. `connectivity_reason` gives the fixed reason code for a failure. `access` is `"owner"` or `"granted"`. Validation checks the key, application and number with GET requests only; it never places a call.
- A call's `transcript_available` is true only when a final transcript turn exists. `recording_available` is true only when the recording is complete. `transcript_available` means only that a final transcript turn exists. It does not mean the transcript is finalized or complete. Before treating a transcript as ready, check the transcript page: `finalized` must be `true`, and `quality_flags` must not include `transcript_in_progress` or `two_party_incomplete`. The transcript endpoint returns a page for any owned call, including one with no turns yet.
- Every `/api/v1` response carries `Cache-Control: no-store`.

## Verifying

```sh
mise exec -- go test -count=1 ./internal/api/ ./internal/service/
```

The OpenAPI drift tests check the documented routes, the error code enum and the call status enum against the code.
