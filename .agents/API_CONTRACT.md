# API and MCP contracts

Public API shall be versioned and documented with OpenAPI; MCP shall use current Streamable HTTP transport and OAuth protected-resource metadata. A stable typed service layer backs both REST and MCP.

Required MCP tools:
- `make_call({profile_id,to,recipient,language,objective,context,instructions,constraints,fallbacks,success_criteria,max_duration_seconds,record,idempotency_key}) -> {call_id,status,created_at}`. `record` is an optional per-call request and MUST NOT enable recording unless the account owner has previously opted in using the WebGUI setting; omission means no recording unless enabled by explicit per-user default policy. No API or MCP caller may bypass an owner-disabled recording setting. Caller may send long context; enforce configurable explicit limits and reject oversized prompts, never truncate invisibly.
- `get_call({call_id}) -> {technical_status,task_outcome,details,transcript_available,recording_available,...}`.
- `list_calls({cursor,filters,limit})`, safe cursor pagination and isolation.
- `get_transcript({call_id,cursor}) -> timed speaker turns and quality flags`.
- `get_recording({call_id}) -> securely authorized download/player resource metadata; actual binary stream over authenticated REST`.
- `get_profiles() -> caller/account names and non-secret connectivity status`.
- `hangup_call({call_id}) -> explicit user-authorized emergency stop and outcome`.

REST mirrors needed UI functions, admin user management, Telnyx/AI configuration validation, data export, retention and theme preferences, read-only self-checks and readiness. Use idempotency keys on commands; no unbounded blocking HTTP request; stable provider/error taxonomy, UTC timestamps, consistent IDs, security audit. Do not expose voice audio as an MCP streaming transport; bridge directly to providers.

## Dual voice protocols
- Voice profile REST/GUI configuration includes an explicit `protocol` enum (`realtime_ws`, `live_ws`) in addition to configurable endpoint/model/voice/headers/secret settings. The selected protocol is persisted per profile and represented in safe non-secret metadata; existing Realtime profiles migrate to `realtime_ws` with no breaking behavior.
- `make_call` resolves the owning profile and starts only the chosen adapter. Any mismatch/unsupported protocol fails visibly; never infer protocol from model ID or issue a hidden paid-model fallback. Do not add a required second model or mid-call MCP roundtrip.

## Transport-independent voice architecture
- Keep `provider_protocol` (Realtime/Live) and `primary_transport` (WebSocket) conceptually separate in service configuration. The REST API MAY expose `primary_transport` only if its value is validated against implemented transports; never advertise WebRTC/SIP as selectable until working and tested. A default/migrated profile chooses WebSocket without a breaking schema change.
- Telnyx telephony configuration is independent of AI protocol configuration. Future provider additions must preserve `make_call`, call IDs, outcome schema, security and retention. Return typed capability errors; do not route incompatible provider/model combinations by silent fallback.
