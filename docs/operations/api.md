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

Only these operations are implemented. Other methods on these paths are not routed. Planned operations such as creating a call are not exposed by this API.

## Listing calls

`GET /api/v1/calls` uses keyset pagination over `(created_at DESC, id DESC)`.

- `limit`: integer from 1 to 100. Defaults to 50. `0`, values above 100, and non-integers return `invalid_input`.
- `cursor`: the opaque `next_cursor` from the previous page. A missing or malformed cursor returns `invalid_input`. An empty `next_cursor` means the last page.
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

## Response conventions

- Timestamps are UTC RFC 3339 with a `Z` suffix, for example `2026-11-25T10:00:00.000Z`.
- `connectivity_status` on telephony profiles is always `"unknown"` until live provider checks ship (issue #81). It is not a health result.
- A call's `transcript_available` is true only when a final transcript turn exists. `recording_available` is true only when the recording is complete.
- Every `/api/v1` response carries `Cache-Control: no-store`.

## Verifying

```sh
mise exec -- go test -count=1 ./internal/api/ ./internal/service/
```

The OpenAPI drift tests check the documented routes, the error code enum and the call status enum against the code.
