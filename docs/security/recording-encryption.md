# Recording encryption

This document describes how CallHarbor stores call recordings on disk. It states what the code does. It is not a formal security proof.

## What is stored

A recording is a two-channel, 16-bit PCM file of one call. Channel 0 is the remote party and channel 1 is the agent audio that was played. Each recording is one `.chrec` file under the data directory, and one `recordings` row in SQLite. Recordings are written only for calls whose owner has opted in and whose call requested recording (see `docs/operations/calls-telnyx.md`).

The file is never plaintext on disk. A WAV file is produced only in memory, when an authorized request reads the audio.

## Keys

- The master key comes from the installation's secret configuration (`CALLHARBOR_MASTER_KEY_FILE` or `CALLHARBOR_MASTER_KEY`), and is versioned. `internal/secrets` holds the keyring.
- Each recording has its own 32-byte random salt, stored in the plaintext file header.
- The per-recording key is derived with HKDF-SHA256 from the master key at the recording's `key_version`, the salt, and the domain-separation label `callharbor recording file key v1`. The derived key is never stored. It is recomputed when the file is read.
- A recording records the `key_version` it was written with. Reads use that version, so a keyring that holds more than one version can read older files.

## File format

The file has a plaintext header, then encrypted segments.

- **Header** (`recording.HeaderSize` bytes): an 8-byte magic `CHRECv01`, the 4-byte big-endian `key_version`, the 32-byte salt, the 4-byte big-endian sample rate, one channel-count byte (always 2), and the 4-byte segment size (always 16 KiB).
- **Segments**: AES-256-GCM in STREAM form. Every non-final segment holds exactly 16 KiB of plaintext. The final segment may be shorter, including empty.
- **Nonce** (12 bytes): a big-endian segment counter, then one flag byte. The flag is `0` for a non-final segment, `1` for the final segment the writer seals, and `2` for an empty final segment that recovery or reseal seals after an authenticated prefix. Flags `1` and `2` are valid only on the last segment, and `2` is never valid elsewhere. A file truncated at a segment boundary therefore cannot pass as complete, segments cannot be reordered, and no counter and flag pair is sealed twice.
- **Associated data**: the magic, the header, the recording id and the owner id, each separated by a zero byte. A file moved to another recording or owner fails authentication.

Because each segment is authenticated on its own, a read can decrypt any segment without reading the whole file. Range reads of the audio use this.

## Access and failure

- Files are created with mode `0600`. The recording directory for each owner is created with mode `0700` and is re-chmodded on creation.
- Every decryption check is an AES-GCM tag check. A corrupted, truncated, moved or re-keyed file fails the tag check. The reader returns an error and never returns the unauthenticated bytes.
- If a read fails after the HTTP response has started, the REST handler aborts the connection. The client sees a truncated download and no success status for the rest of the body. This is documented in `docs/operations/api.md`.
- If no master key is configured, the audio endpoint returns `unavailable` and nothing is decrypted. The metadata endpoint does not decrypt and still answers. Recordings are not deleted in that case.

## Crash recovery

A recording row is written as `recording` when the call starts. If the process stops before the recorder finishes, the row stays in `recording` and the file is left as written.

At startup, `callstream.RecoverInterrupted` settles those rows. It:

1. Opens each file and checks its header. It then walks the segments and authenticates each one in order.
2. Checks for a complete file first. If the last authenticated segment is a final one (the writer's flag, or a previous reseal's), the file is complete. Only bytes after it are trimmed, and the audio is not resealed.
3. Otherwise a torn tail (a short segment that is not a final) is truncated. A full-size segment after the first that fails authentication is damage. The file keeps the authenticated prefix before it and the rest is dropped, because a crash can only leave a shorter tail, so a full-size failure is real damage.
4. Seals the authenticated prefix with an empty resealed final segment (flag 2) at the next counter and fsyncs. The reader accepts it, and the audio stays readable.
5. Closes the row. A recovered row with audio is `partial`, including after damage. A row with no recoverable audio, or a file that is missing, is `failed`. Every recovered row gets the `recovered_after_restart` quality flag. A `failed` row is never served.

A file whose first segment does not authenticate under the configured key (a wrong or rotated master key, an unknown key version, a bad header, or damage to segment 0) is left as it is. Its row stays `recording` and is logged, so a later start with the right key can settle it. A row whose stored path does not match its id is skipped and not finalized.

Damage after segment 0 has no flag of its own. The row is `partial` and carries `recovered_after_restart`, and the recovered audio ends where the damage starts. A new flag was not added to the persisted vocabulary in this change.

A recording whose writer failed during a live call is resealed the same way when the call finishes. Its byte count is the authenticated PCM, and it is `partial` with `write_failed`. If nothing authenticates, it is `failed`.

If no master key is configured, recovery cannot authenticate anything. It leaves the rows and files untouched, logs one warning with the number of unsettled rows, and returns 0. Nothing is finalized or deleted in that case.

## Rotation

Master key rotation is not available in this release. The server builds its keyring with one version, `1`, from the single configured master key (`app.go` calls `NewKeyring(1, ...)`), so no second key version can be configured. Each recording stores its `key_version`, and the file format and key derivation already support several versions, but no command re-encrypts existing recordings under a new key.

Replacing or removing the master key makes every existing recording unreadable. A recording that does not authenticate under the configured key is left in `recording` by the startup sweep, and its file is not modified, so the original key can restore access. Do not change the master key on a server with recordings until a rotation procedure ships. The secret-rotation rule in [secrets.md](../operations/secrets.md#rotation) applies to recordings too.

## Retention and deletion

Recordings have an expiry set from the owner's retention policy at finalize time. Purging expired recordings, and deleting their files, is tracked in issue #72. Until that lands, expired recordings are not removed automatically.

## Limits

- The encryption protects files at rest on the host. It does not protect against a process or operator that can read the master key and the data directory together.
- The writer queue holds 256 plaintext chunks, about one per 20 ms of call audio. A disk stall longer than about 5 s fills it. The recorder then stops writing for the rest of the call, sets `backpressure`, and the row finalizes `partial`. The call itself keeps running. Audio written before the stall stays in the file, and the file closes normally.
- Recording metadata (`recordings` table, quality flags, byte counts, timestamps) is stored in SQLite without application-level encryption.
- The key derivation and AEAD are standard primitives from the Go standard library. This document has not been independently audited.
