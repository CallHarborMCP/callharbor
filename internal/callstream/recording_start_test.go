package callstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/recording"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// startFakeStore is a RecordingStore for startRecording: one optional existing
// row, and failure switches for each write.
type startFakeStore struct {
	existing     *storage.Recording
	inserted     []storage.Recording
	finalized    []storage.RecordingFinal
	reconnected  []string
	formatErr    error
	insertErr    error
	markErr      error
	retentionErr error
}

func (f *startFakeStore) InsertRecording(_ context.Context, r storage.Recording) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserted = append(f.inserted, r)
	return nil
}

func (f *startFakeStore) SetRecordingFormat(context.Context, string, int) error {
	return f.formatErr
}

func (f *startFakeStore) FinalizeRecording(_ context.Context, r storage.RecordingFinal) (bool, error) {
	f.finalized = append(f.finalized, r)
	return true, nil
}

func (f *startFakeStore) OwnerRecordingRetentionDays(context.Context, string) (int, error) {
	if f.retentionErr != nil {
		return 0, f.retentionErr
	}
	return 30, nil
}

func (f *startFakeStore) GetRecordingForCall(context.Context, string, string) (storage.Recording, error) {
	if f.existing == nil {
		return storage.Recording{}, storage.ErrRecordingNotFound
	}
	return *f.existing, nil
}

func (f *startFakeStore) MarkRecordingReconnected(_ context.Context, _, callID, flag string) (bool, error) {
	if f.markErr != nil {
		return false, f.markErr
	}
	f.reconnected = append(f.reconnected, callID+":"+flag)
	return true, nil
}

func startCall() storage.MediaCall {
	return storage.MediaCall{ID: "alice-call", OwnerUserID: "alice", RecordEffective: true}
}

var startLeg = audio.Format{Encoding: audio.L16, SampleRate: 8000, LittleEndian: true}

func startCfg(t *testing.T, store RecordingStore) *Recording {
	t.Helper()
	return &Recording{
		Store: store, Ring: testRing(t), DataDir: t.TempDir(),
		Now: func() time.Time { return time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC) },
	}
}

// A setup failure after the row is inserted finalizes that row as failed. It
// must not stay in 'recording', and the error is returned to the caller.
func TestStartRecordingFormatFailureFinalizesRowFailed(t *testing.T) {
	store := &startFakeStore{formatErr: errors.New("disk on fire")}
	cfg := startCfg(t, store)

	rec, err := startRecording(context.Background(), cfg, startCall(), startLeg, cfg.Now())
	if err == nil || rec != nil {
		t.Fatalf("startRecording = %v, %v; want an error and no recorder", rec, err)
	}
	if len(store.inserted) != 1 {
		t.Fatalf("inserted rows = %d, want 1", len(store.inserted))
	}
	if len(store.finalized) != 1 {
		t.Fatalf("finalize calls = %d, want 1: the row must not stay recording", len(store.finalized))
	}
	got := store.finalized[0]
	if got.ID != store.inserted[0].ID || got.Status != storage.RecordingStatusFailed {
		t.Fatalf("finalize = %+v, want row %s failed", got, store.inserted[0].ID)
	}
	if got.FinalizedAt == "" || got.ExpiresAt == "" {
		t.Fatal("failed row has no finalized_at or expires_at")
	}
}

// A reconnect for a call whose recording is complete marks the row partial and
// appends the reconnect flag. The reconnect creates no second recorder.
func TestStartRecordingReconnectMarksCompleteRecordingPartial(t *testing.T) {
	store := &startFakeStore{existing: &storage.Recording{
		ID: "rec-old", CallID: "alice-call", OwnerUserID: "alice", Status: storage.RecordingStatusComplete,
	}}
	cfg := startCfg(t, store)

	rec, err := startRecording(context.Background(), cfg, startCall(), startLeg, cfg.Now())
	if err != nil || rec != nil {
		t.Fatalf("reconnect = %v, %v; want no recorder and no error", rec, err)
	}
	if len(store.inserted) != 0 {
		t.Fatal("a reconnect created a second recording row")
	}
	want := "alice-call:" + recording.FlagStreamReconnectedNotRecorded
	if len(store.reconnected) != 1 || store.reconnected[0] != want {
		t.Fatalf("reconnect marks = %v, want [%s]", store.reconnected, want)
	}
}

// A reconnect while the first recording is still in progress marks nothing:
// that recorder's own finalize decides the status, and the sweep covers an
// orphan.
func TestStartRecordingReconnectWhileRecordingMarksNothing(t *testing.T) {
	store := &startFakeStore{existing: &storage.Recording{
		ID: "rec-live", CallID: "alice-call", OwnerUserID: "alice", Status: storage.RecordingStatusRecording,
	}}
	cfg := startCfg(t, store)

	if _, err := startRecording(context.Background(), cfg, startCall(), startLeg, cfg.Now()); err != nil {
		t.Fatal(err)
	}
	if len(store.reconnected) != 0 {
		t.Fatalf("marks = %v, want none while the first recording is live", store.reconnected)
	}
}

// An insert failure returns an error and leaves no row behind. The endpoint
// logs that error and runs the call untapped. The endpoint branch itself is not
// driven by a unit test here.
func TestStartRecordingInsertFailureReturnsErrorWithoutRow(t *testing.T) {
	store := &startFakeStore{insertErr: errors.New("constraint")}
	cfg := startCfg(t, store)

	rec, err := startRecording(context.Background(), cfg, startCall(), startLeg, cfg.Now())
	if err == nil || rec != nil {
		t.Fatalf("insert failure = %v, %v; want an error the endpoint can log", rec, err)
	}
	if len(store.finalized) != 0 {
		t.Fatal("a row that was never inserted was finalized")
	}
}
