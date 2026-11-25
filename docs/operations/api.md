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
| `GET` | `/api/v1/calls/{call_id}` | One call owned by the caller |
| `GET` | `/api/v1/telephony-profiles` | Telephony profiles the caller owns or was granted |
| `POST` | `/api/v1/telephony-profiles` | Create a telephony profile (owner) |
| `GET` | `/api/v1/telephony-profiles/{profile_id}` | One usable telephony profile |
| `PUT` | `/api/v1/telephony-profiles/{profile_id}` | Replace a profile's fields and secrets (owner) |
| `DELETE` | `/api/v1/telephony-profiles/{profile_id}` | Delete a profile not referenced by calls (owner) |
| `POST` | `/api/v1/telephony-profiles/{profile_id}/validate` | Read-only connectivity check (owner) |
| `GET` | `/api/v1/telephony-profiles/{profile_id}/grants` | List grants on a profile (admin owner) |
| `PUT` | `/api/v1/telephony-profiles/{profile_id}/grants/{user_id}` | Grant a user use of a profile (admin owner) |
| `DELETE` | `/api/v1/telephony-profiles/{profile_id}/grants/{user_id}` | Revoke a grant (admin owner) |

Only these operations are implemented. Other methods on these paths are not routed. Planned operations such as creating a call are not exposed by this API.

Planned operations are listed in `api/openapi.json` with `x-callharbor-availability: planned`. They are never mounted as operations. Their responses are never a success and never start anything:

- A planned `GET` path (the transcript, recording and recording audio paths) is answered by the API itself. A request needs a valid session, as every `/api/v1` path does. Anonymous requests get `401 unauthenticated`. A signed-in request gets `404` with the `not_found` envelope, `{"error":{"code":"not_found",...}}`. It never reaches the web UI, so the answer is the same whether or not the UI is built.
- A planned `POST` path returns `405 Method Not Allowed` from the router, plain text and not the JSON envelope. Because the web UI's `GET /` pattern matches every path, the router reports a wrong method as 405 rather than 404. For example, `POST /api/v1/calls` returns `405` because `GET /api/v1/calls` is mounted, and `POST /api/v1/calls/{call_id}/transcript` returns `405` as well.

The planned `GET` list is derived from `PlannedRoutes` in `internal/api/v1.go`, and the app tests probe every planned route with a session.

## Telephony profiles

Every telephony route needs a valid session. Each write also needs the session's CSRF token in the `X-CSRF-Token` header; a missing or wrong token returns `403 forbidden`.

- **Secrets are write-only.** `api_key` and the optional `webhook_public_key` are accepted on create and replace. No response, log line or error contains them. On replace, an omitted `api_key` keeps the stored key.
- **Access.** A profile is visible to its owner and to users it was granted to. Anyone else gets `404`, so the existence of another user's profile is not revealed.
- **Grants are admin-only.** Only an admin who owns the profile may list, grant or revoke. A non-admin gets `403` on every grant route, including the owner's.
- **Validation** checks the stored key, the Call Control application (`connection_id`, when set) and the caller number. It sends GET requests only and never places a call. The result is stored as `connectivity_status` (`verified` or `failed`) with a fixed `connectivity_reason` code. The reason codes are `invalid_credentials`, `application_not_found`, `application_inactive`, `number_not_found`, `number_inactive`, `number_not_on_application`, `provider_unreachable` and `secrets_unavailable`. Provider text is never returned.
- **Changes reset validation.** Updating any field sets `connectivity_status` back to `unknown`.
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
| `conflict` | 409 | State conflict |
| `internal` | 500 | Unexpected failure. The message is always `internal error`; details go to the server log only. |

## MCP tool errors

The MCP tools use the same codes and the same `{"error":{"code","message"}}` text as this API. There are two kinds of failure:

- **Service errors** come from the shared service layer: `not_found`, `invalid_input`, `insufficient_scope`, `unauthenticated` and `internal`. The tool result has `isError` set and no structured content, and its text is the envelope.
- **Schema rejections** come from the go-sdk before the handler runs. An unknown `status`, a `limit` outside 1 to 100, or a `cursor` over 512 bytes is refused against the tool's JSON schema. The result has `isError` set, but its text names the rule that failed and is not the service envelope. Clients should check `isError` first, and read the envelope only when the text parses as one.

`internal` always carries the message `internal error`. The cause is logged on the server only.

## Idempotency

Commands that create work take an `Idempotency-Key` header. This convention is part of the planned operations only; no implemented route is a command yet. The key is 1 to 128 characters, must be valid UTF-8, and may not contain whitespace or non-printable characters. An invalid key is rejected with `invalid_input` before any work starts. The format check is `service.ValidateIdempotencyKey`. The replay behavior, where a repeated key returns the original result, is required by the planned operations and is not implemented yet.

## Response conventions

- Timestamps are UTC RFC 3339 with a `Z` suffix, for example `2026-11-25T10:00:00.000Z`.
- `get_profiles` (REST `GET /api/v1/profiles` and the MCP tool of the same name) lists telephony profiles the caller owns **or was granted**. Before this change it listed owned profiles only. A granted profile appears with `access: "granted"`, and it is listed only while its owner is an active admin and the caller is active. Demoting or deactivating the owner hides it from the grantee until the owner is restored.
- `connectivity_status` on telephony profiles is the last read-only validation result: `"unknown"` until a validation runs (and again after any change to the profile), then `"verified"` or `"failed"`. `connectivity_reason` gives the fixed reason code for a failure. `access` is `"owner"` or `"granted"`. Validation checks the key, application and number with GET requests only; it never places a call.
- A call's `transcript_available` is true only when a final transcript turn exists. `recording_available` is true only when the recording is complete.
- Every `/api/v1` response carries `Cache-Control: no-store`.

## Verifying

```sh
mise exec -- go test -count=1 ./internal/api/ ./internal/service/
```

The OpenAPI drift tests check the documented routes, the error code enum and the call status enum against the code.
