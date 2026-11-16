# Architecture — decisions & boundaries

One Go process hosts HTTPS-ready REST/MCP/OAuth handlers, the admin/user React SPA via embedded static files, account and provider integrations, event-driven call state manager, disk streaming recorder, and SQLite WAL persistence. Do not use separate compulsory services. In-process bounded background workers handle retention, reconciliation and cleanup. A second Docker profile `test` can run external protocol simulators for E2E.

## Canonical source layout and mandatory gates
- One Go module at repository root (`go.mod`), service entry `cmd/callharbor/`.
- Business/application packages under `internal/`: `identity/`, `oauth/`, `mcp/`,
  `storage/`, `calls/`, `telephony/`, `realtime/`, `audio/`, `recording/`,
  `security/`, `backup/`. Web frontend is `web/`; public contracts in `api/`.
- Any intentionally different package tree MUST update the relevant contract/E2E tests in the SAME PR. Moving
  packages to an unmonitored directory to bypass a gate is strictly forbidden.
- Before the first real provider integration, satisfy CH-001 protocol-feasibility acceptance criteria with reproducible evidence. This dependency is not currently enforced by GitHub Actions.

## Modules
1. identity: built-in accounts, optional external OIDC, sessions, role and resource permissions; OAuth 2.1 MCP authorization server using trusted library.
2. users/profiles: per-user Telnyx credentials, selected voice protocol (realtime_ws/live_ws), AI endpoint/model/voice, owned caller numbers, defaults and grants; encrypt secrets with managed app master key stored outside SQL. Never print credentials.
3. mcp/api: typed and versioned command/query interface; validation, stable failures, strong authorization, request idempotency, consistent REST/MCP semantics; no direct DB leakage.
4. calls: immutable call brief, state machine, event journal, provider identifiers, retries and precise lifecycle including hangups; reliable reconciliation after restart.
5. telephony: Telnyx Call Control REST adapter, signed webhook verification from raw body, media WebSocket, codecs; extensible provider interface without gratuitous abstraction.
6. realtime: two independent selectable voice protocol adapters (OpenAI-compatible Realtime WebSocket, GPT-Live primary WebSocket) behind a shared call/media interface; each has independent handshake, event handling, audio send/receive, cancellation/barge-in, model/endpoint and usage extraction; no extra planning LLM. Codec conversion PCM/PCMU/L16 with explicit sample rate, endianness and clocks; real conversation supported.
7. recording/transcript: opt-in recording disabled for every new user and installation by default; recording can be enabled/disabled through WebGUI with an explicit acknowledgement dialog about including any legally required notice/consent instructions in the AI call prompt. The application neither injects such notices nor claims legal compliance. Preserve incremental two-channel audio writing with loss/partial handling, authenticated range streaming and durable transcribed turns; independently managed retention. MCP/REST must never override a user-disabled recording setting.
8. storage/audit: SQLite WAL migrations, transactional state, append-only-ish security timeline with access controls; backup/restore compatible with WAL and cipher keys; no external DB.
9. frontend: React+TypeScript accessible responsive UI, theme tokens, cross-platform browser player and navigation; all UI strings are English only in V1 (no localization system), while call conversation language is independently configurable.

## Lifecycle invariants
- `make_call` is asynchronous and idempotent; no duplicate outbound call on retry or crash.
- A provider callback is untrusted until signature/time/replay validation; normalize events and deduplicate with provider identifiers.
- No unbounded goroutines, buffers, per-frame allocations, logs or transient files.
- Record only audio actually sent/received; distinguish generated audio from played audio for accurate barging and recording. Preserve two party channels with timestamp alignment.
- On disconnect/timeout/restart, reconcile remote call status when possible; hang up lingering provider legs defensively. Never report confirmed success from ambiguous provider response.
- OAuth client access tokens are scoped to identity, resource and audience; user identity derived from validated token, never from caller-provided `user_id`.
- Separate trusted instructions (system, local app policy, validated user call brief) from untrusted statements uttered by callee.

## Dependencies/references to evaluate, not blindly copy
- Telnyx official OpenAPI repo (Call Control schema), and telnyx-node/telnyx-go official SDK as appropriate.
- Telnyx official realtime-ai-demo: Telnyx bidirectional media streaming, clear/barge-in/truncate behavior, codec flow.
- Telnyx official telnyx-openclaw-voice-call: provider fake, signed webhook tests, lifecycle and media testing patterns (study source, reimplement for Go, respect licenses).
- Pipecat and LiveKit voice-agent implementations: turn-taking, interruption event ordering, timestamped playout.

Architecture boundary changes require short ADR with alternatives and verified evidence. No extra AI API, PBX, Redis, PostgreSQL, LiveKit runtime or OpenClaw runtime permitted in production.

## Dual voice protocols (V1)
- Preserve the existing Go single-process design, Telnyx Call Control plus bidirectional media WebSocket, and SQLite; no WebRTC/SIP-to-AI dependency or separate mandatory service is introduced.
- `realtime_ws`: connect `/v1/realtime?model=<configured>`; use Realtime session events, per https://developers.openai.com/api/docs/guides/realtime-conversations .
- `live_ws`: connect `/v1/live/sessions` (no query string); immediately send `session.start` containing configurable `model`, initial `instructions`, `audio.format` and output voice; await `session.started`; stream Live `session.input_audio.*` / `session.output_audio.*` as documented and handle `session.close`/`session.closed` finalization. Use real documented event names and JSON schema from https://developers.openai.com/api/reference/resources/live/primary-websocket ; do not assume Realtime messages work unchanged.
- A narrow common media/session interface owns codec conversion, playback markers, graceful call control, audio actually heard, transcript fidelity and results. Keep protocol-specific cancellation/interrupt semantics isolated; assert both with independent test fixtures. A missing capability is a blocker, not a silent fallback or a fabricated transcript.
- Provider profile records explicit protocol, endpoint, model, voice, authentication and timeouts. Upgrade/migrate existing Realtime profiles with explicit `realtime_ws` default; reject ambiguous new configurations.
- Upfront immutable call brief remains the source of instructions for either adapter, with bounded validated context budget and no hidden truncation. GPT-Live is the caller-facing voice agent; its optional delegation is application-controlled and must not require an additional hosted LLM. No paid Responses backend is selected implicitly.
- User-provided proxy endpoints and model aliases are valid configuration, not proof of provider compatibility. Validate startup/event capabilities and report supported features accurately; never silently switch providers or protocols.

## Transport-independent voice architecture (V1)
- Separate three boundaries: `TelephonyAdapter` (dial/hangup, verified callbacks, media transport), `VoiceSession` (protocol handshake, audio, events, interruption, usage, close), and `AudioBridge` (codec clocks, bounded jitter/backpressure, played-vs-generated audio, recording/transcript correlation). The core call state machine consumes normalized events and depends on none of Telnyx-specific JSON, Realtime events or GPT-Live events. Keep interfaces small and behavior-driven; do not create a general-purpose plugin system or new runtime service.
- V1 concrete implementations: Telnyx Call Control + media WS; Realtime primary WS; GPT-Live primary WS. Protocol and transport are orthogonal concerns at the interface level, but do NOT expose unusable UI transport selections. WebRTC primary media and SIP+sideband can be added as tested adapters later with no core rewrite.
- A per-profile capability record reflects actual support for codec/rate, duplex, interruption, transcript fidelity, output playout visibility, usage and delegation. Validate during session setup; fail closed on incompatible profile. Provider aliases/CPA are supported only if their actual protocol and codecs pass verification.
- Normalize raw provider events to durable provenance-preserving internal events; preserve original IDs/metadata separately for debugging. Avoid both a fake lowest-common-denominator interface and uncontrolled provider events leaking into the core. All audio handling must have explicit sample rate and clock domains.
- Measure and document first-audio latency, interruption-to-silence, packet jitter, conversion overhead, CPU and memory for each adapter. Select transport by objective compatibility and performance evidence; WebRTC being recommended does not make it automatically faster for this architecture.
