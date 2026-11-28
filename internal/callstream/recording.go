package callstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/recording"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// RecordingStore is the storage the recorder needs. It is separate from Store so
// the call paths and their fakes do not grow.
type RecordingStore interface {
	InsertRecording(ctx context.Context, r storage.Recording) error
	SetRecordingFormat(ctx context.Context, id string, sampleRate int) error
	FinalizeRecording(ctx context.Context, f storage.RecordingFinal) (bool, error)
	OwnerRecordingRetentionDays(ctx context.Context, ownerUserID string) (int, error)
	GetRecordingForCall(ctx context.Context, ownerUserID, callID string) (storage.Recording, error)
	MarkRecordingReconnected(ctx context.Context, ownerUserID, callID, flag string) (bool, error)
}

// callRecording is one call's recorder with the storage state it finishes.
type callRecording struct {
	rec   *recording.Recorder
	store RecordingStore
	row   storage.Recording
	start time.Time
}

// Recording configures per-call recording. A nil Ring disables recording, and
// so does a call whose RecordEffective is false.
type Recording struct {
	Store RecordingStore
	Ring  *secrets.Keyring
	// DataDir is the root holding the recordings tree.
	DataDir string
	Now     func() time.Time
}

// startRecording opens the recorder for a call whose media stream is attaching.
// It returns nil when the call is not recorded. A second stream for a call that
// already has a recording row creates no new one. A finalized 'complete' row is
// marked partial, since the reconnected audio is not in it.
//
// A failure never aborts the call: the returned error means the call runs
// without a tap. Any row this function inserted is finalized as failed before
// the error returns, so no row is left in 'recording' for a call that is not
// being recorded.
func startRecording(ctx context.Context, cfg *Recording, st storage.MediaCall, leg audio.Format, start time.Time) (*callRecording, error) {
	if cfg == nil || cfg.Ring == nil || !st.RecordEffective {
		return nil, nil
	}
	existing, err := cfg.Store.GetRecordingForCall(ctx, st.OwnerUserID, st.ID)
	switch {
	case err == nil:
		if existing.Status == storage.RecordingStatusComplete {
			if _, err := cfg.Store.MarkRecordingReconnected(ctx, st.OwnerUserID, st.ID, recording.FlagStreamReconnectedNotRecorded); err != nil {
				return nil, fmt.Errorf("mark reconnected recording: %w", err)
			}
		}
		return nil, nil
	case !errors.Is(err, storage.ErrRecordingNotFound):
		return nil, fmt.Errorf("recording lookup: %w", err)
	}
	id, err := newRecordingID()
	if err != nil {
		return nil, err
	}
	var salt [recording.SaltSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, fmt.Errorf("recording salt: %w", err)
	}
	h := recording.Header{KeyVersion: cfg.Ring.Current(), Salt: salt, SampleRate: leg.SampleRate}
	key, err := cfg.Ring.DeriveRecordingKey(h.KeyVersion, salt[:])
	if err != nil {
		return nil, err
	}
	now := cfg.Now()
	w, err := recording.CreateFile(cfg.DataDir, st.OwnerUserID, id, h, key)
	if err != nil {
		return nil, err
	}
	createdAt := now.UTC().Format(storage.TimeLayout)
	row := storage.Recording{
		ID: id, CallID: st.ID, OwnerUserID: st.OwnerUserID,
		StoragePath: recording.RelPath(st.OwnerUserID, id),
		KeyVersion:  h.KeyVersion, CreatedAt: createdAt,
	}
	if err := cfg.Store.InsertRecording(ctx, row); err != nil {
		_ = w.Close()
		// No row points at this file, so it is removed rather than left orphaned.
		if path, perr := recording.Path(cfg.DataDir, st.OwnerUserID, id); perr == nil {
			_ = os.Remove(path)
		}
		return nil, fmt.Errorf("insert recording: %w", err)
	}
	rec, err := recording.New(recording.Config{Leg: leg, Start: now}, w)
	if err != nil {
		_ = w.Close()
		failRecordingRow(ctx, cfg, row, now)
		return nil, err
	}
	if err := cfg.Store.SetRecordingFormat(ctx, id, leg.SampleRate); err != nil {
		_ = rec.Abort()
		// The row exists and its file is open. Finalize it failed so the row is
		// not left in 'recording'. Nothing is served from it.
		failRecordingRow(ctx, cfg, row, now)
		return nil, fmt.Errorf("set recording format: %w", err)
	}
	return &callRecording{rec: rec, store: cfg.Store, row: row, start: now}, nil
}

// Finish seals the recording and writes its terminal row. The caller passes the
// stream end time and whether any agent playout went unconfirmed. Retention is
// read from the owner's policy, so expires_at follows the setting at finalize.
func (c *callRecording) Finish(ctx context.Context, end time.Time, unconfirmed bool, now func() time.Time) error {
	// now stamps finalized_at; end is the media end the audio is cut to.
	// A writer error is already reflected in res: it is partial with the
	// write_failed flag. The error is not a second status, so it is not used.
	res, _ := c.rec.Finish(end, unconfirmed)
	status := res.Status
	if res.ByteCount == 0 {
		// No audio reached the file, so there is nothing to serve.
		status = storage.RecordingStatusFailed
	}
	days, err := c.store.OwnerRecordingRetentionDays(ctx, c.row.OwnerUserID)
	if err != nil {
		return err
	}
	expires, err := storage.RecordingExpiry(c.row.CreatedAt, days)
	if err != nil {
		return err
	}
	_, err = c.store.FinalizeRecording(ctx, storage.RecordingFinal{
		ID: c.row.ID, Status: status, ByteCount: res.ByteCount, DurationMS: res.DurationMS,
		QualityFlags: res.QualityFlags, FinalizedAt: now().UTC().Format(storage.TimeLayout),
		ExpiresAt: expires,
	})
	return err
}

// failRecordingRow finalizes a row this call created but could not start. Its
// file is closed and holds no audio the call can use, so it is failed, not
// served. A finalize error is only logged: the row is then left for the startup
// sweep, which settles any 'recording' row.
func failRecordingRow(ctx context.Context, cfg *Recording, row storage.Recording, now time.Time) {
	days, err := cfg.Store.OwnerRecordingRetentionDays(ctx, row.OwnerUserID)
	if err != nil {
		return
	}
	expires, err := storage.RecordingExpiry(row.CreatedAt, days)
	if err != nil {
		return
	}
	_, _ = cfg.Store.FinalizeRecording(ctx, storage.RecordingFinal{
		ID: row.ID, Status: storage.RecordingStatusFailed,
		FinalizedAt: now.UTC().Format(storage.TimeLayout), ExpiresAt: expires,
	})
}

func newRecordingID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("recording id: %w", err)
	}
	return "rec_" + hex.EncodeToString(b[:]), nil
}
