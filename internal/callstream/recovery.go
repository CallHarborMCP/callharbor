package callstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/recording"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// RecoveryStore is what the startup sweep reads and writes.
type RecoveryStore interface {
	ListRecordingsByStatus(ctx context.Context, status string) ([]storage.Recording, error)
	FinalizeRecording(ctx context.Context, f storage.RecordingFinal) (bool, error)
	OwnerRecordingRetentionDays(ctx context.Context, ownerUserID string) (int, error)
}

// RecoverInterrupted settles every recording left in 'recording' by a previous
// process. It runs once at startup, after migrations and before the server
// accepts traffic, so no live recorder can be racing it.
//
// For each row the file is authenticated as far as it is intact and its torn
// tail is truncated. The row becomes 'partial' with the authenticated audio, or
// 'failed' when no audio authenticated, and it always carries the
// recovered_after_restart flag. A missing file becomes 'failed'. A file that
// does not authenticate at all (damage or a wrong key) is also 'failed' and is
// left on disk for inspection; it is never served.
//
// It returns the number of rows settled. An error on one row is logged and the
// sweep continues, so one bad row cannot block startup.
func RecoverInterrupted(ctx context.Context, store RecoveryStore, ring *secrets.Keyring, dataDir string, now func() time.Time, logger *slog.Logger) (int, error) {
	rows, err := store.ListRecordingsByStatus(ctx, storage.RecordingStatusRecording)
	if err != nil {
		return 0, fmt.Errorf("list interrupted recordings: %w", err)
	}
	if ring == nil {
		// Without a master key no file can be authenticated. Failing the rows
		// here would be irreversible, since a key may be configured later, so
		// they stay 'recording' until a run with a key settles them.
		if len(rows) > 0 && logger != nil {
			logger.Warn("interrupted recordings left unsettled: no master key configured", "count", len(rows))
		}
		return 0, nil
	}
	settled := 0
	for _, r := range rows {
		if err := recoverOne(ctx, store, ring, dataDir, now, r); err != nil {
			if logger != nil {
				logger.Error("recording recovery failed", "recording_id", r.ID, "error", err.Error())
			}
			continue
		}
		settled++
	}
	return settled, nil
}

func recoverOne(ctx context.Context, store RecoveryStore, ring *secrets.Keyring, dataDir string, now func() time.Time, r storage.Recording) error {
	// The stored path must be the one the id derives. A row that names any
	// other file is skipped and left untouched: the sweep never recovers or
	// finalizes a file it cannot place.
	if r.StoragePath != recording.RelPath(r.OwnerUserID, r.ID) {
		return fmt.Errorf("recording %s: storage path does not match its id; skipped", r.ID)
	}
	flags := append([]string{recording.FlagRecoveredAfterRestart}, r.QualityFlags...)
	status := storage.RecordingStatusFailed
	var bytes, duration int64
	sampleRate := r.SampleRate
	res, err := recording.Recover(recording.RecoverInput{
		DataDir: dataDir, RelPath: r.StoragePath, RecordingID: r.ID,
		OwnerID: r.OwnerUserID, KeyVersion: r.KeyVersion, Ring: ring,
	})
	switch {
	case errors.Is(err, recording.ErrMissing):
		// The file is gone: nothing can ever be served, so the row closes.
	case errors.Is(err, recording.ErrCorrupt), errors.Is(err, recording.ErrBadHeader),
		errors.Is(err, secrets.ErrUnknownKeyVersion):
		// Wrong or rotated master key, or damage to segment 0. The key may come
		// back, so the row stays 'recording' and the file is left untouched.
		return fmt.Errorf("recording %s: file does not authenticate under the configured key; left unsettled", r.ID)
	case err != nil:
		return err
	default:
		bytes = res.PlainBytes
		if res.SampleRate > 0 {
			sampleRate = res.SampleRate
		}
		if bytes > 0 && sampleRate > 0 {
			status = storage.RecordingStatusPartial
			duration = bytes / 4 * 1000 / int64(sampleRate)
		}
	}
	days, err := store.OwnerRecordingRetentionDays(ctx, r.OwnerUserID)
	if err != nil {
		return err
	}
	expires, err := storage.RecordingExpiry(r.CreatedAt, days)
	if err != nil {
		return err
	}
	_, err = store.FinalizeRecording(ctx, storage.RecordingFinal{
		ID: r.ID, Status: status, ByteCount: bytes, DurationMS: duration,
		SampleRate:   sampleRate,
		QualityFlags: dedupe(flags),
		FinalizedAt:  now().UTC().Format(storage.TimeLayout), ExpiresAt: expires,
	})
	return err
}

// dedupe keeps the first occurrence of each flag, in order, so a flag already
// recorded is not repeated.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, f := range in {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}
