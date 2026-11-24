# Protocol feasibility evidence: Telnyx, Realtime and GPT-Live

This document is the evidence required by `.agents/PROTOCOL_FEASIBILITY.md`. It makes no architecture decisions. Each claim cites a pinned source and links to an executable test. A claim that is not backed by a source or a test is marked **UNVERIFIED**.

Sources were retrieved on 2026-11-23. **No live Telnyx call and no hosted AI request was made.** Live Telnyx compatibility is **not verified**.

## How to reproduce

```bash
mise exec -- go test -race ./internal/audio/... ./internal/telephony/... ./internal/realtime/... ./internal/recording/...
bash scripts/update-telnyx-contract.sh --check   # requires network; part of verify-local.sh --full
```

## Pinned sources

| ID | Source | Pin | License |
|---|---|---|---|
| T-API | https://github.com/team-telnyx/openapi `openapi/spec3.json` | commit `bf2eb8ed5b816213987fecae8fe33329e98cdd84`, sha256 `fc59cb60…eca18` | **No LICENSE file in the repo** (GitHub API `license: null`). We commit only a mechanical subset as an interface contract, with attribution. See risk R1. |
| T-MS | https://developers.telnyx.com/docs/voice/programmable-voice/media-streaming | retrieved 2026-11-23, no version on the page | docs |
| T-WH | https://developers.telnyx.com/docs/development/api-fundamentals/webhooks/receiving-webhooks | same | docs |
| T-GO | https://github.com/team-telnyx/telnyx-go `lib/webhook_verification.go` | `1eadc27ad02db074efeabe6640f622c830fdf36c` | MIT |
| T-NODE | https://github.com/team-telnyx/telnyx-node `src/lib/webhooks.ts` | `8c94bb302f0188dbdc66908df153ac52169363e1` | MIT |
| T-DEMO | https://github.com/team-telnyx/realtime-ai-demo | `a8c52b0faa30b97ef7c5fb2ea648f048f9b44a23` | **No license.** Facts only; no code was copied. |
| T-CLAW | https://github.com/team-telnyx/telnyx-openclaw-voice-call | `5c817de7a7a45222f5837cdb48396c3c7adf5341` | MIT |
| O-RT | https://developers.openai.com/api/reference/resources/realtime/client-events, …/server-events, https://developers.openai.com/api/docs/guides/realtime-conversations, …/realtime-costs | retrieved 2026-11-23 | docs |
| O-LIVE | https://developers.openai.com/api/reference/resources/live/primary-websocket, https://developers.openai.com/api/docs/guides/voice-websockets, …/live-conversations, …/live-delegation, …/live-migration, …/pricing | retrieved 2026-11-23 | docs |
| O-PY | https://github.com/openai/openai-python `src/openai/types/realtime/*`, `src/openai/types/live/*` | `f63f14b34754fa77fc839ac2cf95da3ef42e1f6a` | Apache-2.0 |

## Executable artifacts

| Artifact | What it proves |
|---|---|
| `api/upstream/telnyx/callcontrol.openapi.json` | Mechanical `$ref`-closed subset of T-API: `POST /calls`, `GET /calls/{id}`, hangup, `streaming_start`/`streaming_stop` and 7 webhooks. Regenerate with `scripts/update-telnyx-contract.sh`; `--check` fails on drift. |
| `internal/telephony/telnyx/contract_test.go` | CallHarbor's dial body validates against the official `CallRequest` JSON Schema for PCMU, PCMA and L16/16k. The official schema rejects 7 corrupted variants. The official webhook examples validate against their official schemas and parse. Malformed webhooks are rejected. |
| `internal/telephony/telnyx/signature_test.go` | Ed25519 over `timestamp|rawBody`, as stated in the T-API header description and implemented in T-GO and T-NODE. Covers a deterministic RFC 8032 key vector, raw-body tamper, reordered JSON, timestamp swap, ±300 s skew, ms/signed timestamps, base64url, short signature, the replay cache, and the media handshake signed over the exact `stream_url` including the query. |
| `internal/telephony/telnyx/media_test.go` | Documented `connected/start/media/stop/mark/dtmf/error` frames, string or number numerics, 18 negative frames, and the exact outbound `media/mark/clear` shapes. It also verifies **played audio after an interruption** against an independent playout emulator (`telnyxfake`). |
| `internal/audio/audio_test.go` | G.711 against ITU reference points and a 256-code round trip, L16 endianness, a 60 s chunked resampler with zero drift (8↔16↔24 kHz), RTP 32-bit wrap/loss/reorder, and stereo two-party alignment with gap fill and horizon bound. |
| `internal/realtime/realtimews/*_test.go` | Verbatim O-RT server events, plus beta and GPT-Live events rejected on a GA session. Uses an independent fake server: `?model=` URL, server-first `session.created`, `session.update` → `session.updated`, truncate → `truncated`. Fails closed if the server ignores the format, sends the wrong first event, the endpoint is GPT-Live, or the endpoint is invalid. |
| `internal/realtime/livews/*_test.go` | Verbatim O-LIVE events. `session.start` is byte-identical in shape to the reference example. Uses an independent fake server: no query, client-first `session.start` → `session.started`, audio, both transcripts, client delegation declined without a backend, and `session.close` → `session.closed` usage. Fails closed on a rewritten model alias, changed format, `responses` delegation, a Realtime server reply, closed-before-start, and close without `session.closed`. |
| `internal/recording/transcript_test.go` | Transcript fidelity: interrupted agent text is flagged and never trimmed or synthesized, and a one-party or unavailable transcript fails completeness. |

## 1. Telnyx Call Control and webhooks

- **Dial**: `POST /v2/calls` with `connection_id`, `to`, `from`, `command_id` (idempotency, per the `HangupRequest`/`CallRequest` description), `webhook_url`, and `stream_url` plus the bidirectional fields. `time_limit_secs` is 30..14400. `stream_establish_before_call_originate=true` so media is ready before answer. Tested in `TestDialRequestMatchesOfficialSchema`.
- **Webhook signature**: headers `telnyx-signature-ed25519` and `telnyx-timestamp`. T-API: "Ed25519 signature of timestamp|payload". The key is a base64 raw 32-byte Ed25519 key. Telnyx documents no tolerance value. 300 s symmetric matches T-GO and T-NODE (a convention, **not a Telnyx requirement**). Replay protection is a bounded cache of signed-message digests kept for 2× the tolerance. Telnyx publishes no static test vector, so `signature_test.go` uses an RFC 8032 TEST 1 key and an independently reproducible signature.
- **Event order**: Telnyx says ordering is not guaranteed (T-MS l.183). Unknown event types are preserved with `Known=false`, not rejected. The state-machine permutation tests belong to the dependent call-engine work (DEV-9). Its fixtures are the official examples committed here.

## 2. Telnyx bidirectional media WebSocket

| Item | Fact | Evidence |
|---|---|---|
| Handshake auth | Same Ed25519 headers, signed over `timestamp|stream_url` (exact URL including query) | T-MS l.102-111; `TestVerifyMediaHandshakeVector` |
| Inbound frames | `connected`, `start` (has `media_format` and top-level `stream_id`), `media` (`track`, `chunk`, `timestamp` = RTP clock units, base64 payload with no RTP header), `stop`, `mark`, `dtmf`, `error{payload.code}` | T-MS; `TestDocumentedMediaFramesParse` |
| Numerics | Strings in the docs. CallHarbor accepts strings and numbers. | T-MS examples |
| Track values | Docs show the placeholder `"inbound/outbound"`. Wire values are `inbound`/`outbound` per Telnyx demos (**UNVERIFIED on the wire**). Anything else is rejected. | T-MS l.171; R1/X1 in research |
| Request tracks | `stream_track` = `inbound_track` (default) / `outbound_track` / `both_tracks`. These are different from the `media.track` wire values. | T-API `CallRequest` |
| Selected mode | `stream_bidirectional_mode=rtp`, codec = inbound codec, `target_legs=self` | T-API enums. `self` follows T-DEMO's report that the default `opposite` targets a non-existent leg on single-leg outbound calls (**EMPIRICAL third-party, UNVERIFIED**). |
| Supported formats | PCMU/8k, PCMA/8k, L16/16k little-endian. G722, OPUS and AMR-WB are valid Telnyx enums but are **rejected** because they are not implemented. | `TestBuildDialRequestRejectsInvalid` |
| Outbound commands | `{"event":"media","media":{"payload":b64}}`, `{"event":"mark","mark":{"name":…}}`, `{"event":"clear"}` | T-MS; `TestOutboundCommandShapes` |
| DTMF | Inbound `dtmf.digit` is a single `0-9*#A-D`. It may arrive out of order (T-MS l.376). | `TestMediaFramesNegative` |
| Errors | 100002–100005 | T-MS l.394-401 |
| Reconnect | Up to 4 attempts, new `connected` + `start`, counters continue, media in the gap is lost | T-MS l.411-435. Behavioural tests belong to DEV-10. |

### Played audio (source of truth)

Telnyx echoes a `mark` after the media queued before it finishes. It **also** echoes a mark immediately when nothing is queued, and returns all queued marks on `clear`. The echo has no field to tell these cases apart (T-MS l.327-358). A mark echo is therefore **not** proof of playback by itself.

CallHarbor's `audio.PlayoutLedger` sends one mark after each media chunk and classifies every echo against its own send history:

- `played`: the echo arrived after real-time playout could have finished and the chunk was not cleared.
- `flushed`: the chunk was cleared.
- `early`: the echo arrived before the audio could have finished. Example: Telnyx dropped the media and echoed immediately.
- missing: no echo within the grace period → `ErrPlayoutTimedOut`.

On `clear`, the elapsed portion of the interrupted chunk is an **estimate** (`PlayedIsEstimate`).

`TestPlayedAudioAfterInterruption` is the measurement. After a barge-in at 330 ms into a 1 s response, the ledger reports 320 ms confirmed and 330 ms estimated. The independent emulator actually played exactly 200+330 ms. The ledger's estimate matches the emulator's played byte count exactly.

`TestEarlyEchoIsNotPlayback` and `TestMissingEchoTimesOut` cover the failure cases.

Whether `outbound_track` includes injected bidirectional audio is **undocumented**, so it is not used.

## 3. Voice protocol capability matrices

Source: `internal/realtime/voice.go`, tested in `TestCapabilityMatrix`.

| Capability | `realtime_ws` (Realtime GA) | `live_ws` (GPT-Live primary WS) |
|---|---|---|
| Connect | `wss://…/v1/realtime?model=M`, `Authorization: Bearer`, no `OpenAI-Beta` | `wss://…/v1/live/sessions`, **no query**, `Authorization: Bearer` |
| Startup | Server sends `session.created` first → client `session.update` → `session.updated` | Client sends `session.start` first (model, instructions, audio, voice, delegation) → `session.started`. No audio before it. |
| Formats | `audio/pcm` 24 kHz only, `audio/pcmu`, `audio/pcma` (8 kHz implied, not stated). Input and output are set separately. | `audio/pcm` 16k or 24k, `audio/pcmu`/`pcma` at 8k. One format for both directions. No server resampling. |
| Telnyx match without resampling | PCMU/PCMA 8k ↔ Telnyx PCMU/PCMA 8k | PCMU/PCMA 8k, or L16 16k ↔ Telnyx L16 16k (LE: **UNVERIFIED**, see R4) |
| Turn detection | `server_vad`/`semantic_vad`, `interrupt_response` | Model-internal. Not configurable. |
| Caller transcript | **Opt-in, separately billed transcription model** (`audio.input.transcription`). O-RT costs: "If enabled, input transcription is billed separately". Asynchronous, no word timing. | `session.input_transcript.delta` is always on, with `start_ms`/`end_ms` (approximate, session-relative). No separate transcription charge is documented (absence of a charge: **UNVERIFIED**). |
| Agent transcript | `response.output_audio_transcript.delta/.done`. No timing. | `session.output_transcript.delta` with `start_ms`/`end_ms` |
| Interruption | Client `conversation.item.truncate{item_id,audio_end_ms}` → `conversation.item.truncated` (no text). `response.cancel`. | No truncate or cancel event. The client must stop playout itself. |
| Playout events | `output_audio_buffer.*` is WebRTC/SIP only, so unavailable on WS | None |
| Delegation | n/a (tools are not used) | `session.delegation.created`. The client answers with `session.commentary.append` ("unavailable"). No backend. |
| Usage | `response.done.response.usage` (tokens) | `session.usage.updated.usage.seconds` (cumulative). Final value in `session.closed.usage`. |
| Close | Socket close | `session.close` → `session.closed{reason,usage}`. Socket close without `session.closed` means usage is unconfirmed. |
| Limits | 60 min session | Instructions ≤16,384 tokens. Startup input ≤128 msgs / 8,192 tokens. Model, voice, format, instructions and delegation type are immutable after start. |

## 4. Transcript fidelity: findings and V1 decisions

1. **Dual-party transcription on `realtime_ws`: not available; resolved by owner decision (option a).**
   - The only in-protocol source of caller speech text is input transcription. O-RT states it runs on a separate transcription model billed separately ("If enabled, input transcription is billed separately by the Realtime API … uses a different model and rate card"; e.g. `gpt-transcribe` $0.0045/min).
   - `.agents/PRODUCT_SCOPE.md` and `.agents/PROTOCOL_FEASIBILITY.md` §5 forbid silently enabling billable STT.
   - CallHarbor therefore never sets `audio.input.transcription`, and `Capabilities.Check(TwoPartyTranscript)` fails closed for `realtime_ws` (`TestRealtimeTwoPartyTranscriptFailsClosed`).
   - No standards-compliant, no-extra-service alternative exists inside the Realtime protocol: the model's own response text is not a transcript of the caller.
   - **V1 decision (CEO, 2026-11-24, option a):** `realtime_ws` profiles record caller audio, and the caller transcript is shown as `unavailable` (fail closed). There is **no paid STT fallback** and no per-profile transcription opt-in in V1; option (b) would change the locked scope. In code, `Capabilities.V1CallerTranscript()` returns `unavailable` for `realtime_ws`, and `recording.CallerEntry(…, available=false, …)` yields an empty `unavailable` entry, so `recording.Complete` never reports a full two-party transcript for these calls (`TestRealtimeTwoPartyTranscriptFailsClosed`, `internal/recording/transcript_test.go`).
   - `live_ws` is not affected: both transcripts are native (`TestSessionLifecycleAndTwoPartyTranscript`).
2. **Audible agent text after barge-in: no word alignment on either protocol.**
   - Realtime truncation "doesn't provide a truncated transcript" (O-RT realtime-conversations).
   - Live has no truncation, and its `start_ms`/`end_ms` cover whole fragments only.
   - CallHarbor therefore keeps the provider text unmodified and flags it `interrupted_text_includes_unplayed`, with estimated played duration. It never trims by guesswork (`TestAgentEntryFidelity`).
   - A verbatim audible-only agent transcript is **not obtainable** from either protocol. This is a documented limitation, not something inferred.
3. Neither protocol reports playout. Telnyx marks plus the ledger in §2 are the only played-audio evidence.

## 5. Security and network notes

- Webhooks: verify the raw body before parsing, with a 1 MiB body bound, 300 s skew and replay cache. Unknown events are recorded, not trusted for state.
- Media WSS: verify the signature over the exact `stream_url`. Use an unguessable per-call path. `stream_auth_token` delivery is undocumented (R5), so it is not relied upon.
- AI endpoints: `wss` only. Plaintext `ws` is allowed only to loopback emulators. Credentials in URLs are rejected. Live rejects any query.
- Public reachability, reverse proxy and SSRF policy for user-supplied endpoints are deployment concerns owned by later work. They are not proven here.

## 6. Unresolved upstream ambiguities

| # | Ambiguity | Source | Reproduction / handling |
|---|---|---|---|
| R1 | team-telnyx/openapi has no LICENSE | `GET https://api.github.com/repos/team-telnyx/openapi/license` → 404 / `license: null` | Only a mechanical interface subset is committed, with attribution. The owner should confirm that is acceptable. |
| R2 | Mark echo is indistinguishable for played vs. empty-queue vs. cleared, and RTP-mode mark support is not explicit (T-DEMO relies on it, T-CLAW says "Telnyx does not use mark events") | T-MS l.327-358 | Ledger classification and timeout. Needs a live check. |
| R3 | `media.track` wire values (`inbound/outbound` placeholder) | T-MS l.171 | Strict `inbound`/`outbound`. Needs a live check. |
| R4 | L16 byte order on the WS (T-DEMO reports little-endian, contrary to RFC 3551) | T-DEMO README l.321 | Pinned to LE in the dial format. Needs a live check. |
| R5 | How `stream_auth_token` reaches the WS server | T-MS (absent) | Not relied upon. |
| R6 | Whether `outbound_track` includes injected audio | T-MS, T-API | Not used. |
| R7 | `target_legs` for single-leg outbound (`self` in T-DEMO vs `both` in T-CLAW) | T-DEMO `src/telnyx/api.ts` l.46-48 | `self`. Needs a live check. |
| R8 | Valid codec × rate combinations: spec enum 8000/16000/22050/24000/48000 vs doc table 8k/16k | T-API `StreamBidirectionalSamplingRate` vs T-MS l.266-273 | Only 8k G.711 and 16k L16 are allowed. |
| R9 | `enable_reconnect` absent from T-API; `AnswerRequest` lacks some stream fields | T-API | Not sent. Reconnect defaults to on (T-MS). |
| R10 | Realtime truncate deletes "unplayed portion" (guide) vs "server-side text transcript" (reference) | O-RT | CallHarbor never relies on server text after a truncate. |
| R11 | Realtime GA G.711 sample rate not stated in the schema | O-RT, O-PY `realtime_audio_formats.py` | 8 kHz assumed, and verified by `session.updated` echo at runtime. |
| R12 | Live `session.output_audio.delta` example has `start_ms`/`end_ms`, but the schema says they are omitted on the primary WS | O-LIVE | Timing is ignored on audio. |
| R13 | Live `error.code` is a required string in the schema, but the guide mentions `null` codes | O-LIVE live-conversations | Both accepted (test). |
| R14 | Live `usage.seconds` is an integer in the reference, a float in O-PY | O-LIVE, O-PY `session_usage.py` | Parsed as float (test). |
| R15 | Live pre-start error, close codes, max session length, whether output stops immediately on barge-in | O-LIVE (absent) | Fail closed on any non-`session.started` first event. Barge-in silencing is local (Telnyx `clear`). GPT-Live sends no speech-start event in the pinned fixtures, so `input_audio_buffer.speech_started` is rejected fail-closed (`livews` negative test); the bridge cuts playout on an explicit local call, not a provider signal. GPT-Live audio events carry no item ID, so a barge-in opens a synthetic local turn: the bridge closes it on cut (Telnyx `clear`) and suppresses all agent audio without an item ID until the first `session.input_transcript.delta` (caller speech) after the cut, which reopens the turn. Identified items stay gated per item. Signed-stream test `TestSignedStreamBargeInSuppressesStreamingLiveAudio` asserts no `media` after `clear` while the fake keeps streaming, and that audio resumes only after a caller transcript; it fails with the gate disabled. Live turn-taking after a barge-in therefore waits for caller speech. The limit that GPT-Live does not signal speech start remains live-unverified. |
| R16 | Whether GPT-Live transcription carries any extra charge | O-LIVE pricing (silent) | Must be confirmed by the owner from billing before claiming "no extra cost". |
| R17 | Arbitrary model aliases or gateways (e.g. `gpt-live-1-codex`) | PRODUCT_SCOPE | Supported only if the handshake echoes the same model and format. Otherwise fail closed (`model alias rewritten` test). |

## 7. Simulation limits

All emulators (`telnyxfake`, the fake Realtime server and the fake Live server) are independent implementations of the **documented** behaviour. They cannot prove undocumented live behaviour (R2–R7, R15). Real first-audio latency, jitter, interruption-to-silence timing and live codec behaviour remain unmeasured until an owner-run billable acceptance call. `Live Telnyx tested` stays **false**.
