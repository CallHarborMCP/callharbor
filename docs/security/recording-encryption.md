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
- **Nonce** (12 bytes): a big-endian segment counter, then one flag byte. The flag is `1` only on the final segment. A file truncated at a segment boundary therefore cannot pass as complete, and segments cannot be reordered.
- **Associated data**: the magic, the header, the recording id and the owner id, each separated by a zero byte. A file moved to another recording or owner fails authentication.

Because each segment is authenticated on its own, a read can decrypt any segment without reading the whole file. Range reads of the audio use this.

## Access and failure

- Files are created with mode `0600`. The recording directory for each owner is created with mode `0700` and is re-chmodded on creation.
- Every decryption check is an AES-GCM tag check. A corrupted, truncated, moved or re-keyed file fails the tag check. The reader returns an error and never returns the unauthenticated bytes.
- If a read fails after the HTTP response has started, the REST handler aborts the connection. The client sees a truncated download and no success status for the rest of the body. This is documented in `docs/operations/api.md`.
- If no master key is configured, the recording read endpoints return `unavailable` and nothing is decrypted. Recordings are not deleted in that case.

## Crash recovery

A recording row is written as `recording` when the call starts. If the process stops before the recorder finishes, the row stays in `recording` and the file is left as written.

At startup, `callstream.RecoverInterrupted` settles those rows. It:

1. Opens each file and checks its header. It then walks the segments and authenticates each one.
2. Truncates a torn tail: a short final segment that does not authenticate is cut off, and the file keeps the authenticated prefix. A full-size segment that fails authentication is not treated as a torn write. The file is refused, so the row is closed as failed and the file is left for inspection.
3. Closes the row. A recovered row with audio is `partial`, and a row with no recoverable audio, or a refused file, is `failed`. Every recovered row gets the `recovered_after_restart` quality flag. A `failed` row is never served.

If no master key is configured, recovery cannot authenticate anything. It leaves the rows and files untouched, logs one warning with the number of unsettled rows, and returns 0. Nothing is finalized or deleted in that case.

## Rotation

Master key rotation is not automated. A new key version can be added to the keyring, and new recordings use it. Existing recordings keep their original `key_version` and remain readable while that version is in the keyring. Re-encrypting old recordings under a new key is not implemented. Removing a key version makes its recordings unreadable.

## Retention and deletion

Recordings have an expiry set from the owner's retention policy at finalize time. Purging expired recordings, and deleting their files, is tracked in issue #72. Until that lands, expired recordings are not removed automatically.

## Limits

- The encryption protects files at rest on the host. It does not protect against a process or operator that can read the master key and the data directory together.
- Recording metadata (`recordings` table, quality flags, byte counts, timestamps) is stored in SQLite without application-level encryption.
- The key derivation and AEAD are standard primitives from the Go standard library. This document has not been independently audited.
