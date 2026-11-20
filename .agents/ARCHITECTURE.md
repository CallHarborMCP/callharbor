# Architecture — decisions & boundaries

One Go process hosts HTTPS-ready REST/MCP/OAuth handlers, the admin/user React SPA via embedded static files, account and provider integrations, event-driven call state manager, disk streaming recorder, and SQLite WAL persistence. Do not use separate compulsory services. In-process bounded background workers handle retention, reconciliation and cleanup. A second Docker profile `test` can run external protocol simulators for E2E.

## Canonical source layout and mandatory gates
- One Go module at repository root (`go.mod`), service entry `cmd/callharbor/`.
- Business/application packages under `internal/`: `identity/`, `oauth/`, `mcp/`,
  `storage/`, `calls/`, `telephony/`, `realtime/`, `audio/`, `recording/`,
  `security/`, `backup/`. Web frontend is `web/`; public contracts in `api/`.
- Any intentionally different package tree MUST update the relevant contract/E2E tests in the SAME PR. Moving
  packages to an unmonitored directory to bypass a gate is strictly forbidden.
- Before the first real provider integration, satisfy `.agents/PROTOCOL_FEASIBILITY.md` with reproducible evidence.

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

## Voice protocol and transport boundaries

- V1 keeps Telnyx Call Control/media WebSocket, one Go process and SQLite while supporting two distinct primary server-side voice WebSocket protocols: `realtime_ws` and `live_ws`. Their event schemas, startup handshakes and termination are not interchangeable; detailed protocol requirements live in `.agents/PRODUCT_SCOPE.md` and executable feasibility evidence in `.agents/PROTOCOL_FEASIBILITY.md`.
- Keep three focused interfaces: `TelephonyAdapter` (dial, hangup, verified callbacks and media transport), `VoiceSession` (protocol handshake, audio/events, interruption, usage and close) and `AudioBridge` (codec clocks, bounded jitter/backpressure, played versus generated audio and transcript/recording correlation). The core call state machine receives normalized events, never provider-specific JSON. Avoid a generic plugin framework or another production service.
- Preserve codec/rate/clock conversions, playout acknowledgement, cancellation, interruption, reliable close, transcript provenance and bounded queues. Retain raw provider IDs and diagnostic metadata separately.
- Record and validate real per-profile capabilities: codecs/rates, duplex, interruption, transcript fidelity, playout visibility, usage and delegation. Reject incompatible endpoint/model/alias combinations visibly rather than silently switching protocols, transports or paid backends.
- Supply immutable, validated instructions and long-form context up front. Do not require a mandatory extra planning model or mid-call MCP request. Live delegation, if offered, is application controlled and must not silently add paid APIs.
- WebRTC and SIP remain future extension points; do not implement or advertise them in V1. Measure real first-audio latency, interruption-to-silence, jitter, conversion overhead and resource usage for each supported adapter before making transport-performance claims.
