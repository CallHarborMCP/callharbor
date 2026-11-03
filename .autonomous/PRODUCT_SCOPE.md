# CallHarbor V1.0.0 — Locked Product Contract

## Product mission
Build a fully usable production-grade, self-hosted, lightweight platform for autonomous outbound AI telephone calls. Not an MVP, not a prototype, not an MCP-only wrapper. All the functionality specified here must ship together in v1.0.0. Autonomous implementation must not expand scope or silently defer mandatory capabilities.

## Guiding principles
- Multi-user; strict per-user identity, tenant-like resource isolation, no leaks via REST, MCP, files, logs, WebSocket or IDs.
- One container: Go modular monolith with embedded built React/TypeScript web UI, SQLite WAL and local encrypted-at-rest storage of secrets/recordings as appropriate. No required external DB, queue, cache, hosted service, agent runtime or paid LLM API.
- Runtime must remain lightweight, measured under idle, one call, several calls; do NOT hard-code arbitrary memory caps, trade off correctness/security for RAM, or introduce large dependent frameworks.
- Telephony provider in V1: Telnyx Call Control and bidirectional Media Streaming (not mandatory SIP PBX). Internal interfaces extensible to other operators in V2+ without implementing them.
- Realtime provider in V1: user-configurable **OpenAI-compatible speech-to-speech Realtime WebSocket** endpoint, model, auth, voice, timeouts. Do not mention or depend on any particular private proxy product in user-facing code/config/docs. Default must NOT imply all text-only OpenAI-compatible proxies support Realtime audio.
- MCP client prepares the goal, recipient, context, constraints, fallbacks and success criteria BEFORE dialing. Voice agent conducts the conversation autonomously with no heavy planning model or client MCP roundtrips mid-call. Conversation must be truly duplex, adapt to human interruptions, and preserve conversational context.
- Only phone-minute charges from user's chosen telephony provider; no added hosted MCP/AI SaaS required. No hidden paid API fallback.

## Users and authentication
- Exactly two roles: `admin` and `user`. More than one admin allowed. Role controls enforced server-side for WebGUI/API/MCP/downloads.
- Built-in identity/accounts for standalone deployment plus optional OpenID Connect login via any standards-compatible provider (PocketID is not mandatory). Secure bootstrap first admin; no hard-coded default credentials. Disable/deactivate users, revoke sessions and tokens.
- Built-in OAuth 2.1 authorization server for remote ChatGPT/MCP clients: Authorization Code + PKCE S256, authorization-server/resource metadata, dynamic client registration or verified client-id metadata as applicable, proper resource/audience/scopes, refresh/revocation and OAuth consent. Follow currently published MCP/ChatGPT connector compatibility requirements. Use maintained, audited libraries; no homegrown cryptography or protocol shortcuts.
- Users may configure their own Telnyx API key, connection/application, webhook verification public key, caller number(s), AI gateway/voice profiles and preference. Admins may set defaults/shared profiles with explicit grants. Each profile belongs to an owner; secrets never rendered again or logged.
- Admin has global management/audit capability; user can access only their own resources. Admin accesses other users' content only through explicit auditable admin actions.

## WebGUI and appearance
- React + TypeScript, professional accessible component system (e.g. Tailwind + shadcn/ui/Radix, or comparable mature maintained alternative), responsive from phone through desktop; no merely scaled desktop tables.
- Default visual direction: understated Apple-like polish; not generic AI-generated gradients/dashboard clones. Clear typography, consistent layout, coherent navigation, keyboard and screen reader support.
- **Appearance settings in V1**: light/dark/system, meaningful design theme presets, accent color, density/spacing. Persist per user. Visual-regression screenshot coverage for desktop/mobile, light/dark, theme variants; responsive breakpoints and touch-friendly controls.
- **WebGUI language in V1**: ENGLISH ONLY, including navigation, onboarding, user/admin screens, forms, validation, error/success messages, dialogs, recording warning, date labels and accessibility text. No i18n engine, translation bundles or language selector are required. This does NOT constrain speech-to-speech conversation languages or the per-call `language` parameter; the AI assistant must be able to speak the requested language (e.g. Czech).
- Screens: first-run setup/admin wizard; sign-in; dashboard; call list/filter/search; call detail with complete prompt, event timeline, transcript with speaker/timestamps, outcome and error detail; inline audio playback with pause, seeking, timeline, download; user profiles/config; admin users/role management, config, operational diagnostics and security audit.
- Usage/cost reporting must distinguish confirmed provider amounts from estimates; never invent a tariff. Graceful handling when prices unavailable.

## Telephone call lifecycle
- MCP `make_call` input: owning profile, E.164 `to`, recipient, locale/language, objective, long context, instructions, constraints, allowed fallbacks, success criteria, max duration, recording preference, idempotency key. Version the validated immutable call brief before dialing; secret/PII handling and redaction in logs.
- No hard-coded arbitrary low prompt size; explicit validated upper limit, error signaling, token budget and prompt injection boundaries. A recipient speaking on a phone cannot override trusted authorization, call limits or tool policy.
- MCP tools: `make_call`, `get_call`, `list_calls`, `get_transcript`, `get_recording` (authorized metadata/URL only), `get_profiles`, `hangup_call` (emergency safety stop only). End-to-end HTTP Streamable MCP with OAuth.
- `make_call` returns a stable call identifier quickly; no MCP request open for duration of call. Polling/status is supported. Stable error codes and idempotency under retries.
- Technical status machine (queued, initiating, ringing, active, ending, ended, failed), separately tracked task outcome (success, partial, failed, unknown) with supporting evidence; unexpected disconnect or unavailable reliable transcript must not be interpreted as success.
- Autonomous speech-to-speech conversational assistant: real-time interruption/barge-in, speaker turn detection, playback queue clearing, contextual continuation, DTMF if supported, graceful hangup, caller silence/voicemail/busy/no-answer, reconnection/timeouts, bounded cost/duration and cancellation. No midcall GUI interventions in V1 (only emergency hangup).
- Preserve full event timeline, causality/correlation IDs, timestamps, state transitions, Telnyx IDs and safe diagnostics. Handle duplicate/reordered events; provider signatures; prevent accidental duplicate dialing.

## Recordings, transcripts, retention
- Optional **local** recording of both directions without Telnyx's paid recording service. Capture correct media timing and both parties, save incrementally without RAM buffering entire call; browser-playable WAV initially, including seeking and download. Clearly signal partial/incomplete recordings on failure. Secure authorization on bytes and range requests; never expose public object paths.
- Recording OFF by default for each new account and new installation. Per-user WebGUI enable/disable toggle with a mandatory explicit warning/acknowledgement before OFF→ON: users must ensure their AI call prompt contains any notice/consent wording required for their circumstances. This warning is advisory, not a claim of legal compliance, and the application does not insert an announcement or implement country-specific rules. An owner-disabled setting cannot be overridden by MCP `record=true`, REST requests, admin defaults or migration. A call may record only after account-level opt-in, with per-call opt-out supported.
- User-configurable recording retention; default proposal 30 days IF recording is enabled; independent transcript and audit/event retention configurable by admin with per-user settings bounded by policy. Correctly delete files and metadata; expired data not silently retained forever in backups or temp artifacts. Admin UI for retention and disk usage.
- Transcript should indicate speakers, timestamps, live-to-final correction if available; if realtime provider cannot supply truthful full two-party transcript, record the limitation, fail a mandatory recording/transcript contract until implemented with no additional paid AI API, and never hallucinate content.
- Access to private recordings, call notes and sensitive credentials must be audited. Use a carefully documented key management, authenticated encryption and rotation strategy without homemade crypto. Restore/backup tests must cover decryption and retention.
- Jurisdiction-specific recording notices, automated announcements and consent workflows are **OUT OF SCOPE**. An advisory notice shown only when enabling recording is IN SCOPE. No forced pre-call announcements or per-country legal behavior. Users may put relevant wording in their supplied call instructions; the app does not make legal-compliance guarantees.

## Non-functional / deployment
- Single self-hosted Docker image containing Go binary and embedded React app; one mapped persistent data directory; Compose example and reverse-proxy guide. Build reproducibly in CI; publish signed/checksummed Linux amd64+arm64 multi-platform artifact/manifest and GHCR image tagged `v1.0.0` and `latest` only after verifying each platform and the published manifest.
- External exposure: secure callback and media WebSocket endpoints reachable from Telnyx; all other app/API/MCP/OAuth routes securely configured; user should be able to put WebGUI behind an ingress/reverse proxy. No assumption NetBird or PocketID is installed.
- No fixed RAM budget: benchmark actual idle, 1 and multiple concurrent calls, CPU, disk and latency, document. Bound concurrency with per-user/admin limits and backpressure. Detect degraded provider connections; no stuck billable calls.
- Full production behaviors: installation wizard, migration safety, health/readiness, structured logs (redacted), backup/restore CLI or UI, upgrade rollback approach, robust clock/timezone behavior, clean shutdown.
- Persistent user data stays on Docker volume; avoid docker.sock, privileged mode and unnecessary open ports. Multi-stage build, minimal nonroot final image, read-only root filesystem where feasible.

## V1 exclusions (planned follow-ups)
- Live human intervention/steering during conversation (beyond emergency hangup); human takeover; inbound AI receptionist; additional telephony providers; outbound campaigns/bulk dialer; complex enterprise call queues; country-specific legal notices/recording policies. All belong in roadmap V2+; modular interfaces must make them possible without implementing dead abstractions now.
