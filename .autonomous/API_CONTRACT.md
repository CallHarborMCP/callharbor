# API and MCP contracts

Public API shall be versioned and documented with OpenAPI; MCP shall use current Streamable HTTP transport and OAuth protected-resource metadata. A stable typed service layer backs both REST and MCP.

Required MCP tools (V1):
- `make_call({profile_id,to,recipient,language,objective,context,instructions,constraints,fallbacks,success_criteria,max_duration_seconds,record,idempotency_key}) -> {call_id,status,created_at}`. `record` is an optional per-call request and MUST NOT enable recording unless the account owner has previously opted in using the WebGUI setting; omission means no recording unless enabled by explicit per-user default policy. No API or MCP caller may bypass an owner-disabled recording setting. Caller may send long context; enforce configurable explicit limits and reject oversized prompts, never truncate invisibly.
- `get_call({call_id}) -> {technical_status,task_outcome,details,transcript_available,recording_available,...}`.
- `list_calls({cursor,filters,limit})`, safe cursor pagination and isolation.
- `get_transcript({call_id,cursor}) -> timed speaker turns and quality flags`.
- `get_recording({call_id}) -> securely authorized download/player resource metadata; actual binary stream over authenticated REST`.
- `get_profiles() -> caller/account names and non-secret connectivity status`.
- `hangup_call({call_id}) -> explicit user-authorized emergency stop and outcome`.

REST mirrors needed UI functions, admin user management, Telnyx/AI configuration validation, data export, retention and theme preferences, read-only self-checks and readiness. Use idempotency keys on commands; no unbounded blocking HTTP request; stable provider/error taxonomy, UTC timestamps, consistent IDs, security audit. Do not expose voice audio as an MCP streaming transport; bridge directly to providers.
