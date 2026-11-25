# Remote MCP authorization (built-in OAuth 2.1)

CallHarbor includes an OAuth 2.1 authorization server so remote MCP clients can connect on behalf of a signed-in CallHarbor user. This page covers configuration, the endpoints and protocol features that exist today, and what has and has not been verified.

`/mcp` is a stateless Streamable HTTP MCP endpoint that answers JSON responses. It registers three read-only tools: `get_profiles`, `list_calls` and `get_call`. Each tool runs as the OAuth principal, so a call owned by another user is reported as `not_found`. The planned operations `make_call`, `get_transcript`, `get_recording` and `hangup_call` are not registered. A call to any of them fails as an unknown tool and never starts, ends or reads anything. Issue #67 delivers this; later issues add the planned operations. See `api/mcp/tools.json` for the contract.

## Enable it

Set the public origin that clients and browsers use to reach CallHarbor:

| Variable | Example | Rules |
| --- | --- | --- |
| `CALLHARBOR_PUBLIC_URL` | `https://callharbor.example.com` | Absolute origin. `https` is required, except `http` on a loopback host (`127.0.0.1`, `[::1]`, `localhost`) for local testing. No path, query, fragment or userinfo. The value is normalized: a trailing `/` is removed, the host is lowercased, and a default port (`:443` for `https`, `:80` for `http`) is removed. |

- The issuer is exactly this origin. The protected MCP resource is `<origin>/mcp`.
- The issuer is never derived from `Host` or `X-Forwarded-*` headers. Behind a reverse proxy, set the variable to the external `https` origin.
- The rate limits (authorization and registration) key on the client address. By default that is the TCP peer, and `X-Forwarded-For` is ignored. Behind a reverse proxy, list the proxy in `CALLHARBOR_TRUSTED_PROXIES` (see below) so each real client gets its own limit. Limits are per process and reset on restart.
- When the variable is unset, no OAuth or MCP route is mounted and the server logs `OAuth and MCP are disabled until CALLHARBOR_PUBLIC_URL is set`. Local sign-in and the web UI keep working.
- No master key is needed for OAuth. Access tokens, refresh tokens and authorization codes are random opaque values; only their SHA-256 hashes are stored in SQLite.

### Behind a reverse proxy

`CALLHARBOR_TRUSTED_PROXIES` is a comma-separated list of IP addresses and CIDR prefixes, for example `10.0.0.0/8, 192.0.2.7`. Whitespace is ignored. It is empty by default, which trusts no proxy. An invalid entry, an empty entry or a zoned address (such as `fe80::1%eth0`) stops the server at startup, and the message names the variable.

- The server reads `X-Forwarded-For` only when the TCP peer is in the list. It walks the header from right to left, across all header values, and uses the first address that is not in the list as the client.
- If the peer is not trusted, the header is ignored. If the header is missing, malformed or zoned, or every entry is a trusted proxy, the TCP peer is used.
- Each `X-Forwarded-For` entry must be a bare IP address, such as `198.51.100.7` or `2001:db8::7`. An entry that carries a port, such as `198.51.100.7:5000` or `[2001:db8::7]:5000`, is malformed. The whole header is then ignored and the request uses the shared TCP peer bucket. Configure the proxy to send bare addresses.
- `X-Real-IP` and `Forwarded` are never read.
- Add only the addresses of the proxies that connect to CallHarbor, and make the proxy set `X-Forwarded-For` by appending the address it received the connection from. A wrong list is a security issue. If a client can reach CallHarbor directly, or the list contains a range such as `0.0.0.0/0` or `::/0` that covers clients, that client can write any address into the header and pick its own rate-limit bucket. If the proxy is missing from the list, every client shares the proxy's bucket and one busy client can exhaust the limit for all.
- The external origin in `CALLHARBOR_PUBLIC_URL` is separate from this setting and is never derived from forwarded headers.

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
| `GET /.well-known/oauth-authorization-server` | RFC 8414 authorization server metadata |
| `GET /oauth/authorize` | Authorization endpoint; redirects to the consent screen `/oauth/consent` |
| `POST /oauth/token` | `authorization_code` and `refresh_token` grants |
| `POST /oauth/revoke` | RFC 7009 token revocation |
| `POST /oauth/register` | RFC 7591 dynamic client registration |
| `POST /mcp`, `GET /mcp`, `DELETE /mcp` | Protected MCP Streamable HTTP endpoint (JSON responses, 1 MiB body limit) |
| `OPTIONS /mcp` | CORS preflight, answered without credentials |

Metadata, token, revocation and registration endpoints send `Access-Control-Allow-Origin: *` without credentials so browser-based clients can use them. No other route sends CORS headers.

## Protocol behavior

- **Authorization code + PKCE:** only `response_type=code`. PKCE with `S256` is mandatory; a missing challenge or `plain` is rejected. Codes live 60 seconds and are single use. Presenting a used code fails with `invalid_grant` and revokes every token issued from it.
- **Resource indicators (RFC 8707):** the authorization request must carry exactly one `resource`, equal to `<origin>/mcp`, otherwise `invalid_target`. A `resource` sent to the token endpoint must match the grant. Tokens are bound to that audience and `/mcp` rejects any other audience.
- **Issuer identification (RFC 9207):** every authorization response, success or error, carries `iss=<origin>`, and the metadata advertises `authorization_response_iss_parameter_supported`.
- **Scopes:** `mcp:read` (view calls, transcripts, recordings and profiles) and `mcp:call` (place and stop calls). Unknown scopes are rejected with `invalid_scope`, not dropped. Omitting `scope` requests both. A token without a required scope gets `403` with `error="insufficient_scope"`.
- **Clients:** public clients only (`token_endpoint_auth_method=none`); no client secrets are issued.
  - *Client ID Metadata Documents:* an `https` `client_id` URL with a path is fetched and must contain the same `client_id`. A non-`none` singular `token_endpoint_auth_method` is accepted when `none` is listed in `token_endpoint_auth_methods_supported`; the client is always treated as public, and dynamic registration still requires `none`. The fetch allows public IP addresses only, follows no redirects, times out after 5 seconds, accepts at most 64 KiB of `application/json`, and is cached for the `Cache-Control` max-age (default 5 minutes, at most 1 hour).
    - A `client_id` longer than 2048 bytes is refused before any fetch. This matches the database limit.
    - A failed fetch is remembered for 30 seconds (negative cache), so a bad URL does not cause one outbound request per attempt. The negative cache holds at most 1024 entries; expired entries are removed on each new failure, and the oldest is dropped when it is full.
    - Concurrent outbound fetches are limited to 8 across all clients. A fetch that cannot get a slot within the 5-second timeout is refused as an unknown client.
    - Each fetched document is written to the client table before it is cached, so a cached client always has a row. At most 10,000 metadata documents are mirrored. At that cap, a new document evicts the oldest metadata row that has not been updated for at least one hour and that no grant, request, code or token references. If no row qualifies, the new document is refused. A document already mirrored is always accepted.
  - *Dynamic registration:* limited to 20 registrations per 10 minutes per client IP and 10,000 dynamic clients in total. For IPv6 the limit applies per /64 prefix, because one host usually controls a whole /64; IPv4 is limited per address. Registered names are unverified, and the consent screen says so.
    - At the total cap, a new registration evicts the oldest dynamic client that is more than 24 hours old and that no grant, authorization request, code or token references. The eviction is written to the server log with the client IDs only. If no client qualifies, the registration is refused with `503 temporarily_unavailable`. Clients that are in use, or were registered within the last 24 hours, are never evicted.
- **Authorization rate limit:** `GET /oauth/authorize` allows 60 requests per minute per client IP (IPv6 per /64). A limited request gets `429` with `Retry-After` and `temporarily_unavailable`, and no redirect is sent.
- **Cleanup:** on each authorization request, up to 100 expired codes are deleted, then expired authorization requests that no code references. Expired or revoked tokens and grants are not purged; they are kept as audit history and for reuse detection.
- **Password reset:** a password reset revokes all of the user's OAuth grants in the same transaction, as deactivation does. The connected client must be authorized again.
- **Redirect URIs:** exact string match against the registered list. Each must be `https`, or `http` on a loopback host. Fragments, userinfo and wildcards are rejected. For a loopback URI only the port may differ from a registered URI, because a native app picks a free port at runtime (RFC 8252, section 7.3). A different host, scheme, or path is refused. An unknown client or unregistered redirect URI produces an error page, never a redirect.
- **Tokens:** access tokens last 1 hour. Refresh tokens rotate on every use and each rotation sets a new 30-day expiry, so an active client's refresh lifetime slides. There is no absolute grant lifetime: a client that refreshes at least once every 30 days keeps its grant indefinitely, until it is revoked, its user is deactivated, or its password is reset. Reusing a rotated refresh token revokes the whole grant.
- **Accounts:** consent requires a CallHarbor session and the CSRF token. Identity comes only from the validated token, never from request parameters. Deactivating a user revokes all of their OAuth grants in the same transaction, and every token use checks that the user is still active.
- **Audit:** client registration, consent decisions, grant revocation and refresh-token reuse are written to the audit log. Tokens, codes and verifiers are never logged.

## Implementation

The server uses `github.com/zitadel/oidc/v3` for protocol types and checks, and implements the rest itself:

- **From the library:** request and error types, the redirect-URI, response-type and code-challenge-method validators, the loopback check, the PKCE verifier comparison, and the RFC 8414 discovery struct.
- **In this repository:** token issuance, refresh rotation and reuse detection, grant storage, bearer validation on `/mcp`, client ID metadata document resolution, and dynamic client registration. Tokens and codes come from `crypto/rand` and are stored only as SHA-256 hashes.

The library's full OpenID Provider is not used. It signs ID tokens with a JWT key that this server does not need, and it does not provide RFC 8707 resource binding or client ID metadata documents, so those are implemented here.

## Verification status

Tested automatically in CI (`go test`, Playwright). Most cases run through the real application or real HTTP handlers against SQLite:

- The full remote-client flow through the running server: discovery, dynamic registration, sign-in, consent, code exchange with an independent `golang.org/x/oauth2` client, refresh rotation and revocation.
- Negative cases: wrong or missing resource, audience mismatch between two instances, insufficient scope, redirect URI mismatch, code reuse and concurrent redemption, missing or `plain` PKCE, wrong verifier, revoked tokens, refresh reuse, disabled users (including users disabled with their grants left live), denied consent, cross-user grant access, and missing session or CSRF on the consent API.
- Token expiry is tested at package level with an injected fake clock, not by waiting in the running application.
- Client ID Metadata Documents served by a local TLS test server. The ChatGPT case uses the exact bytes of the document published at `https://chatgpt.com/oauth/client.json` as of 2026-11-25, with only `client_id` changed to the test server's URL. It is accepted as a public client that uses `none`, although the document names `private_key_jwt`, because `none` is in its `token_endpoint_auth_methods_supported` list. Its unsupported `jwt-bearer` grant is dropped, and refresh tokens still issue. The token endpoint never authenticates a client by assertion. Negative cases cover a singular `private_key_jwt` with no `none` support, a supported list without `none`, and a grant list without `authorization_code`.
- The consent screen and Connected apps page in a browser against the production build.

**Not verified:** a connection from the ChatGPT or Claude user interfaces, or any other hosted MCP client. The ChatGPT fixture test checks only that the published document is accepted by this server. Connecting from ChatGPT also needs a publicly reachable `https` origin. The MCP transport (#67) is in place; the go-sdk client is verified end to end in this repository.
