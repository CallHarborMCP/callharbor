# Secrets and the master key

CallHarbor stores user telephony and AI API keys encrypted at rest. Each secret is sealed with AES-256-GCM under one **master key** that you supply. The master key is never stored in the database, never logged and never returned by any API. This guide documents only behavior that exists in the current code.

## Key management: what you must do

1. **Generate a key.** A 32-byte random value, base64-encoded:

   ```sh
   openssl rand -base64 32
   ```

2. **Store it in a file with strict permissions** owned by the user the server runs as (UID 65532 in the Docker image). Create the file under a restrictive umask so it is never readable by others:

   ```sh
   (umask 077 && printf '%s' 'PASTE_BASE64_KEY_HERE' > /etc/callharbor/master.key)
   chown 65532:65532 /etc/callharbor/master.key
   chmod 0400 /etc/callharbor/master.key
   ```

   Then point the server at it:

   ```sh
   CALLHARBOR_MASTER_KEY_FILE=/etc/callharbor/master.key
   ```

3. **Fallback: environment variable.** `CALLHARBOR_MASTER_KEY` holds the same base64 value directly. It works, but environment values are more widely visible (process listings, container inspection). Prefer the file.

If both are set, the file is used and the environment variable is ignored.

## Startup behavior (fail closed)

| Situation | Result |
| --- | --- |
| No key set, no stored secrets | Server starts. Writing a secret returns `503 secrets_unavailable`. |
| No key set, stored secrets exist | **Server refuses to start.** Set the key, or restore it from backup. |
| Key present but not valid base64, or not 32 bytes | Server refuses to start. The error never prints the key. |
| Key present | Secrets can be written and read. |

The server will not run with credentials it cannot decrypt, and it never writes a secret in clear text.

## Backup

The master key and the database must be backed up **together and kept consistent with each other**:

- A database backup **without** the matching master key cannot be decrypted.
- Keep the master key in a separate, access-controlled location from the database backup, so one leak does not expose both.
- Test a restore: start the server against the restored database and the restored key, then confirm a known secret resolves.

## Losing the key

**If the master key is lost, every stored secret is unrecoverable.** There is no recovery path and CallHarbor cannot decrypt the rows without it. Users would have to re-enter their Telnyx and AI API keys. Treat the key file as critical: back it up, restrict its access, and record where the copy lives.

## Rotation

The stored format records a key version for every secret, and the server can decrypt rows under more than one version. However, **this release has no command that re-encrypts existing rows under a new key.** Do not change or replace the master key on a server with stored secrets until a rotation procedure ships; doing so makes the existing secrets unreadable.

When a rotation procedure is available, this section will be updated with the exact steps.

## AI profile fields

Each AI voice profile has these fields. Only the API key is secret. The rest is stored and returned in plain text.

| Field | Stored | Notes |
| --- | --- | --- |
| `protocol` | plain | `realtime_ws` or `live_ws`. Explicit; never inferred from the model or URL. |
| `endpoint_url` | plain | `wss://` only. `ws://` is accepted only for a loopback address. |
| `model` | plain | Model identifier sent to the provider. |
| `voice` | plain | Voice identifier sent to the provider. |
| `instructions` | plain | Up to 8192 bytes. Do not put secrets here. |
| `headers` | plain | Non-secret custom headers. **Do not put credentials here** (see below). |
| `api_secret_id` | reference | Points to the encrypted `ai_api_key` secret. The key value is never in the profile. |

### Custom headers are not secret

Headers are stored and returned in plain text. Use them only for non-secret values such as a tenant or routing label. Do not put an API key, token or password in a header.

The API refuses `Authorization` and the WebSocket and hop-by-hop headers. That guard does not catch every credential, so the rule is still yours to follow: put the provider key in the secret field, never in a header.

### Endpoint safety (SSRF)

Provider endpoints are checked at connection time against the resolved IP address, not only the name in the URL. By default, connections to private, loopback, link-local and cloud-metadata addresses are refused, and redirects are not followed. This blocks an owner from pointing a profile at an internal host.

A self-hosted gateway on a private network needs an explicit, admin-approved deployment policy. No setting for that exists in this release, so such endpoints will not connect yet.

## What the API will never do

- No endpoint returns a stored secret value. `GET /api/secrets` returns metadata only (id, name, purpose, timestamps).
- Secrets are created with `POST /api/secrets` and require the session CSRF token.
- AI profile headers are not encrypted and are returned on read. Keep credentials out of them.
- Users can only see and use their own secrets. Another user's secret id returns not-found.
- Telephony profile keys are stored as secrets with the purposes `telnyx_api_key` and `telnyx_webhook_public_key`. They are sealed like any other secret, and they are never returned by the telephony API.

## Verifying the setup

After starting the server with a key, check the secret API from the web UI or with an authenticated session, create a test secret, and confirm it appears in the list without its value.
