# Remote MCP authorization (built-in OAuth 2.1)

CallHarbor includes an OAuth 2.1 authorization server so remote MCP clients can connect on behalf of a signed-in CallHarbor user. This page covers configuration, the endpoints and protocol features that exist today, and what has and has not been verified.

The MCP transport itself (Streamable HTTP and the call tools) is tracked separately in issue #67. Until it ships, an authorized request to `/mcp` returns `501` with a JSON-RPC error `-32601` ("MCP transport is not available yet"). It never reports success for an operation that does not exist.

## Enable it

Set the public origin that clients and browsers use to reach CallHarbor:

| Variable | Example | Rules |
| --- | --- | --- |
| `CALLHARBOR_PUBLIC_URL` | `https://callharbor.example.com` | Absolute origin. `https` is required, except `http` on a loopback host (`127.0.0.1`, `[::1]`, `localhost`) for local testing. No path, query, fragment or userinfo. A trailing `/` is removed. |

- The issuer is exactly this origin. The protected MCP resource is `<origin>/mcp`.
- The issuer is never derived from `Host` or `X-Forwarded-*` headers. Behind a reverse proxy, set the variable to the external `https` origin.
- When the variable is unset, no OAuth or MCP route is mounted and the server logs `OAuth and MCP are disabled until CALLHARBOR_PUBLIC_URL is set`. Local sign-in and the web UI keep working.
- No master key is needed for OAuth. Access tokens, refresh tokens and authorization codes are random opaque values; only their SHA-256 hashes are stored in SQLite.

## Connect a client

Give the MCP client the server URL `<origin>/mcp`. A conforming client then:

1. Calls `/mcp` without a token and receives `401` with `WWW-Authenticate: Bearer resource_metadata="<origin>/.well-known/oauth-protected-resource/mcp", scope="mcp:read mcp:call"`.
2. Reads the protected resource metadata, then the authorization server metadata.
3. Identifies itself with a Client ID Metadata Document URL, or registers dynamically.
4. Opens the authorization URL in the user's browser. The user signs in to CallHarbor with their own account, reviews the consent screen and chooses **Allow** or **Deny**.
5. Exchanges the code for tokens and calls `/mcp` with `Authorization: Bearer <token>`.

Each user can see and revoke connected clients under **Connected apps** in the web UI. Revocation takes effect on the next request.

## Endpoints

| Endpoint | Purpose |
| --- | --- |
| `GET /.well-known/oauth-protected-resource/mcp` (also the root form) | RFC 9728 protected resource metadata |
| `GET /.well-known/oauth-authorization-server`, `GET /.well-known/openid-configuration` | RFC 8414 authorization server metadata (same document) |
| `GET /oauth/authorize` | Authorization endpoint; redirects to the consent screen `/oauth/consent` |
| `POST /oauth/token` | `authorization_code` and `refresh_token` grants |
| `POST /oauth/revoke` | RFC 7009 token revocation |
| `POST /oauth/register` | RFC 7591 dynamic client registration |
| `POST /mcp`, `GET /mcp` | Protected MCP resource (placeholder until #67) |

Metadata, token, revocation and registration endpoints send `Access-Control-Allow-Origin: *` without credentials so browser-based clients can use them. No other route sends CORS headers.

## Protocol behavior

- **Authorization code + PKCE:** only `response_type=code`. PKCE with `S256` is mandatory; a missing challenge or `plain` is rejected. Codes live 60 seconds and are single use. Presenting a used code fails with `invalid_grant` and revokes every token issued from it.
- **Resource indicators (RFC 8707):** the authorization request must carry exactly one `resource`, equal to `<origin>/mcp`, otherwise `invalid_target`. A `resource` sent to the token endpoint must match the grant. Tokens are bound to that audience and `/mcp` rejects any other audience.
- **Issuer identification (RFC 9207):** every authorization response, success or error, carries `iss=<origin>`, and the metadata advertises `authorization_response_iss_parameter_supported`.
- **Scopes:** `mcp:read` (view calls, transcripts, recordings and profiles) and `mcp:call` (place and stop calls). Unknown scopes are rejected with `invalid_scope`, not dropped. Omitting `scope` requests both. A token without a required scope gets `403` with `error="insufficient_scope"`.
- **Clients:** public clients only (`token_endpoint_auth_method=none`); no client secrets are issued.
  - *Client ID Metadata Documents:* an `https` `client_id` URL with a path is fetched and must contain the same `client_id`. The fetch allows public IP addresses only, follows no redirects, times out after 5 seconds, accepts at most 64 KiB of `application/json`, and is cached for the `Cache-Control` max-age (default 5 minutes, at most 1 hour).
  - *Dynamic registration:* limited to 20 registrations per 10 minutes per client IP and 10,000 registered clients in total (`503 temporarily_unavailable` when full). Registered names are unverified, and the consent screen says so.
- **Redirect URIs:** exact string match against the registered list. Each must be `https`, or `http` on a loopback host. Fragments, userinfo and wildcards are rejected. An unknown client or unregistered redirect URI produces an error page, never a redirect.
- **Tokens:** access tokens last 1 hour. Refresh tokens last up to 30 days and rotate on every use. Reusing a rotated refresh token revokes the whole grant.
- **Accounts:** consent requires a CallHarbor session and the CSRF token. Identity comes only from the validated token, never from request parameters. Deactivating a user revokes all of their OAuth grants in the same transaction, and every token use checks that the user is still active.
- **Audit:** client registration, consent decisions, grant revocation and refresh-token reuse are written to the audit log. Tokens, codes and verifiers are never logged.

## Verification status

Tested automatically in CI (`go test`, Playwright) against the real application and SQLite:

- The full remote-client flow through the running server: discovery, dynamic registration, sign-in, consent, code exchange with an independent `golang.org/x/oauth2` client, refresh rotation and revocation.
- Negative cases: wrong or missing resource, audience mismatch between two instances, insufficient scope, redirect URI mismatch, code reuse and concurrent redemption, missing or `plain` PKCE, wrong verifier, expired and revoked tokens, refresh reuse, disabled users, denied consent, cross-user grant access, and missing session or CSRF on the consent API.
- Client ID Metadata Documents served by a local TLS test server, including a document shaped like ChatGPT's published client metadata.
- The consent screen and Connected apps page in a browser against the production build.

**Not verified:** a connection from the ChatGPT or Claude user interfaces, or any other hosted MCP client. The ChatGPT-shaped test checks only the document format. Connecting from ChatGPT also needs the MCP transport (#67) and a publicly reachable `https` origin.
