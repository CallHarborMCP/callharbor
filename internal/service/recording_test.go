package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/recording"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// fakeRecordings is an in-memory RecordingStore with the owner rules of the
// storage layer: a call with no row for this owner reads as not found.
type fakeRecordings struct {
	calls   map[string]string // call id -> owner
	rows    map[string]storage.Recording
	audits  []storage.AuditEvent
	auditEr error
}

func (f *fakeRecordings) GetRecordingForCall(_ context.Context, owner, callID string) (storage.Recording, error) {
	r, ok := f.rows[callID]
	if !ok || f.calls[callID] != owner {
		return storage.Recording{}, storage.ErrRecordingNotFound
	}
	return r, nil
}

func (f *fakeRecordings) InsertAudit(_ context.Context, e storage.AuditEvent) error {
	if f.auditEr != nil {
		return f.auditEr
	}
	f.audits = append(f.audits, e)
	return nil
}

// recordingFixture bundles the service, its fake store, and the master keyring
// and data directory a real encrypted file lives in.
type recordingFixture struct {
	svc     *Service
	store   *fakeRecordings
	ring    *secrets.Keyring
	dataDir string
}

// newRecordingFixture builds the service over fakeStore, which answers GetCall
// with the owner rules, and over fakeRecordings. Calls are seeded with an owner
// each; the foreign-call cases add a call owned by someone else.
func newRecordingFixture(t *testing.T) *recordingFixture {
	t.Helper()
	ring, err := secrets.NewKeyring(1, map[int][]byte{1: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	calls := []storage.Call{
		{ID: "c-alice", OwnerUserID: "alice"},
		{ID: "c-bob", OwnerUserID: "bob"},
	}
	fr := &fakeRecordings{
		calls: map[string]string{"c-alice": "alice", "c-bob": "bob"},
		rows:  map[string]storage.Recording{},
	}
	dataDir := t.TempDir()
	svc := New(&fakeStore{calls: calls}).WithRecordings(fr, ring, dataDir)
	svc.now = func() time.Time { return time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC) }
	return &recordingFixture{svc: svc, store: fr, ring: ring, dataDir: dataDir}
}

// writeRecording writes a finalized recording file with the given PCM bytes and
// returns its row. The file is real: a header, then encrypted segments.
func (f *recordingFixture) writeRecording(t *testing.T, callID, id string, pcm []byte) storage.Recording {
	t.Helper()
	h := recording.Header{KeyVersion: 1, SampleRate: 8000}
	for i := range h.Salt {
		h.Salt[i] = byte(i + 1)
	}
	key, err := f.ring.DeriveRecordingKey(h.KeyVersion, h.Salt[:])
	if err != nil {
		t.Fatal(err)
	}
	w, err := recording.CreateFile(f.dataDir, "alice", id, h, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(pcm); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	row := storage.Recording{
		ID: id, CallID: callID, OwnerUserID: "alice",
		Status:      storage.RecordingStatusComplete,
		StoragePath: recording.RelPath("alice", id),
		ByteCount:   int64(len(pcm)), DurationMS: 1000,
		KeyVersion: 1, SampleRate: 8000, Channels: 2,
		QualityFlags: []string{recording.FlagAgentPlayoutEstimated},
		CreatedAt:    "2026-11-27T08:00:00.000Z",
		FinalizedAt:  "2026-11-27T08:00:01.000Z",
	}
	f.store.rows[callID] = row
	return row
}

func aliceActor() Actor {
	return Actor{UserID: "alice", Scopes: []string{ScopeRead}}
}

func errorCodeOf(t *testing.T, err error) Code {
	t.Helper()
	if err == nil {
		t.Fatal("want error, got nil")
	}
	return AsError(err).Code
}

func TestGetRecordingViewOfCompleteRecording(t *testing.T) {
	f := newRecordingFixture(t)
	f.writeRecording(t, "c-alice", "rec_one", make([]byte, 400))

	v, err := f.svc.GetRecording(context.Background(), aliceActor(), "c-alice")
	if err != nil {
		t.Fatal(err)
	}
	if v.CallID != "c-alice" || v.Status != storage.RecordingStatusComplete || v.Partial {
		t.Fatalf("status view = %+v", v)
	}
	if v.ContentType != "audio/wav" || v.Channels != 2 || v.SampleRate != 8000 {
		t.Fatalf("format view = %+v", v)
	}
	if strings.Join(v.ChannelLayout, ",") != "remote_party,agent" {
		t.Fatalf("channel layout = %v", v.ChannelLayout)
	}
	if v.ByteCount != 44+400 {
		t.Fatalf("byte_count = %d, want 444 (header plus PCM)", v.ByteCount)
	}
	if v.DownloadPath != "/api/v1/calls/c-alice/recording/audio" {
		t.Fatalf("download_path = %q", v.DownloadPath)
	}
	if v.QualityFlags == nil || len(v.QualityFlags) != 1 {
		t.Fatalf("quality_flags = %v", v.QualityFlags)
	}
	if v.FinalizedAt == nil || *v.FinalizedAt != "2026-11-27T08:00:01.000Z" {
		t.Fatalf("finalized_at = %v", v.FinalizedAt)
	}
	if len(f.store.audits) != 1 || f.store.audits[0].Action != "recording.read" || f.store.audits[0].Outcome != "success" {
		t.Fatalf("audits = %+v", f.store.audits)
	}
}

func TestGetRecordingForeignAndMissingAreIdenticalNotFound(t *testing.T) {
	f := newRecordingFixture(t)
	f.writeRecording(t, "c-alice", "rec_one", make([]byte, 40))
	_, foreign := f.svc.GetRecording(context.Background(), Actor{UserID: "bob", Scopes: []string{ScopeRead}}, "c-alice")
	_, missing := f.svc.GetRecording(context.Background(), aliceActor(), "c-nope")
	if errorCodeOf(t, foreign) != CodeNotFound || errorCodeOf(t, missing) != CodeNotFound {
		t.Fatalf("codes = %v %v, want not_found for both", foreign, missing)
	}
	if AsError(foreign).Message != "call not found" || AsError(missing).Message != "call not found" {
		t.Fatalf("messages differ: %q %q", AsError(foreign).Message, AsError(missing).Message)
	}
	for _, e := range f.store.audits {
		if e.Outcome != "denied" {
			t.Fatalf("refusal audit outcome = %q, want denied", e.Outcome)
		}
	}
}

func TestGetRecordingNotAvailableCases(t *testing.T) {
	cases := map[string]func(*recordingFixture){
		"no row": func(f *recordingFixture) {},
		"failed": func(f *recordingFixture) {
			f.store.rows["c-alice"] = storage.Recording{ID: "r", CallID: "c-alice", OwnerUserID: "alice", Status: storage.RecordingStatusFailed}
		},
		"deleted": func(f *recordingFixture) {
			f.store.rows["c-alice"] = storage.Recording{ID: "r", CallID: "c-alice", OwnerUserID: "alice", Status: storage.RecordingStatusDeleted}
		},
		"complete with no audio": func(f *recordingFixture) {
			f.store.rows["c-alice"] = storage.Recording{ID: "r", CallID: "c-alice", OwnerUserID: "alice", Status: storage.RecordingStatusComplete, ByteCount: 0}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRecordingFixture(t)
			setup(f)
			_, err := f.svc.GetRecording(context.Background(), aliceActor(), "c-alice")
			if errorCodeOf(t, err) != CodeNotFound || AsError(err).Message != "recording not available" {
				t.Fatalf("err = %v, want not_found recording not available", err)
			}
			if len(f.store.audits) != 1 || f.store.audits[0].Outcome != "denied" {
				t.Fatalf("audits = %+v", f.store.audits)
			}
		})
	}
}

func TestGetRecordingInProgressIsMetadataWithZeroBytes(t *testing.T) {
	f := newRecordingFixture(t)
	f.store.rows["c-alice"] = storage.Recording{
		ID: "r", CallID: "c-alice", OwnerUserID: "alice", Status: storage.RecordingStatusRecording,
		CreatedAt: "2026-11-27T08:00:00.000Z", QualityFlags: nil,
	}
	v, err := f.svc.GetRecording(context.Background(), aliceActor(), "c-alice")
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != "recording" || !v.Partial || v.ByteCount != 0 || v.FinalizedAt != nil {
		t.Fatalf("in-progress view = %+v", v)
	}
	if v.QualityFlags == nil {
		t.Fatal("quality_flags must be an empty array, not null")
	}
}

func TestGetRecordingRequiresScopeAndSession(t *testing.T) {
	f := newRecordingFixture(t)
	if _, err := f.svc.GetRecording(context.Background(), Actor{}, "c-alice"); errorCodeOf(t, err) != CodeUnauthenticated {
		t.Fatalf("anonymous err = %v", err)
	}
	if _, err := f.svc.GetRecording(context.Background(), Actor{UserID: "alice"}, "c-alice"); errorCodeOf(t, err) != CodeInsufficientScope {
		t.Fatalf("no-scope err = %v", err)
	}
}

func TestGetRecordingUnavailableWithoutWiring(t *testing.T) {
	svc := New(&fakeStore{calls: []storage.Call{{ID: "c-alice", OwnerUserID: "alice"}}})
	if _, err := svc.GetRecording(context.Background(), aliceActor(), "c-alice"); errorCodeOf(t, err) != CodeUnavailable {
		t.Fatalf("unwired err = %v, want unavailable", err)
	}
}

func TestGetRecordingAuditFailureFailsRequest(t *testing.T) {
	f := newRecordingFixture(t)
	f.writeRecording(t, "c-alice", "rec_one", make([]byte, 40))
	f.store.auditEr = errors.New("disk full")
	_, err := f.svc.GetRecording(context.Background(), aliceActor(), "c-alice")
	if errorCodeOf(t, err) != CodeInternal {
		t.Fatalf("audit failure err = %v, want internal", err)
	}
}

func TestOpenRecordingAudioStreamsHeaderThenPCM(t *testing.T) {
	f := newRecordingFixture(t)
	pcm := make([]byte, 3*recording.SegmentSize/2+8)
	for i := range pcm {
		pcm[i] = byte(i * 7)
	}
	f.writeRecording(t, "c-alice", "rec_big", pcm)

	open, err := f.svc.OpenRecordingAudio(context.Background(), aliceActor(), "c-alice", "bytes=0-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = open.Close() }()
	body, err := io.ReadAll(open.Audio)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 44+len(pcm) {
		t.Fatalf("body = %d bytes, want %d", len(body), 44+len(pcm))
	}
	if string(body[:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		t.Fatalf("header = %q", body[:12])
	}
	if string(body[44:]) != string(pcm) {
		t.Fatal("PCM does not round-trip through the decrypting stream")
	}
	if open.View.ByteCount != int64(44+len(pcm)) {
		t.Fatalf("view byte_count = %d", open.View.ByteCount)
	}
	if last := f.store.audits[len(f.store.audits)-1]; last.Action != "recording.audio_read" || last.Outcome != "success" {
		t.Fatalf("audit = %+v", last)
	}
}

func TestOpenRecordingAudioSeekAcrossSegmentBoundary(t *testing.T) {
	f := newRecordingFixture(t)
	pcm := make([]byte, 2*recording.SegmentSize+100)
	for i := range pcm {
		pcm[i] = byte(i % 251)
	}
	f.writeRecording(t, "c-alice", "rec_seek", pcm)
	open, err := f.svc.OpenRecordingAudio(context.Background(), aliceActor(), "c-alice", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = open.Close() }()

	// A window that spans the first segment boundary of the PCM.
	start := int64(44 + recording.SegmentSize - 10)
	if _, err := open.Audio.Seek(start, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 40)
	n, err := io.ReadFull(open.Audio, buf)
	if err != nil || n != 40 {
		t.Fatalf("read = %d, %v", n, err)
	}
	want := pcm[recording.SegmentSize-10 : recording.SegmentSize+30]
	if string(buf) != string(want) {
		t.Fatal("window across the segment boundary is wrong")
	}
}

func TestOpenRecordingAudioInProgressIsConflict(t *testing.T) {
	f := newRecordingFixture(t)
	f.store.rows["c-alice"] = storage.Recording{
		ID: "r", CallID: "c-alice", OwnerUserID: "alice", Status: storage.RecordingStatusRecording,
	}
	_, err := f.svc.OpenRecordingAudio(context.Background(), aliceActor(), "c-alice", "")
	if errorCodeOf(t, err) != CodeConflict || AsError(err).Message != "recording in progress" {
		t.Fatalf("err = %v, want conflict recording in progress", err)
	}
}

func TestOpenRecordingAudioWithoutKeyringIsUnavailable(t *testing.T) {
	f := newRecordingFixture(t)
	f.writeRecording(t, "c-alice", "rec_nokey", make([]byte, 40))
	f.svc.recordings.ring = nil
	_, err := f.svc.OpenRecordingAudio(context.Background(), aliceActor(), "c-alice", "")
	if errorCodeOf(t, err) != CodeUnavailable {
		t.Fatalf("no-key err = %v, want unavailable", err)
	}
}

func TestOpenRecordingAudioRejectsPathOutsideIDLayout(t *testing.T) {
	f := newRecordingFixture(t)
	row := f.writeRecording(t, "c-alice", "rec_path", make([]byte, 40))
	row.StoragePath = "alice/../elsewhere.chrec"
	f.store.rows["c-alice"] = row
	_, err := f.svc.OpenRecordingAudio(context.Background(), aliceActor(), "c-alice", "")
	if errorCodeOf(t, err) != CodeInternal {
		t.Fatalf("mismatched path err = %v, want internal", err)
	}
}

func TestOpenRecordingAudioMissingFileIsInternal(t *testing.T) {
	f := newRecordingFixture(t)
	row := f.writeRecording(t, "c-alice", "rec_gone", make([]byte, 40))
	if err := os.Remove(filepath.Join(f.dataDir, row.StoragePath)); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.OpenRecordingAudio(context.Background(), aliceActor(), "c-alice", "")
	if errorCodeOf(t, err) != CodeInternal {
		t.Fatalf("missing file err = %v, want internal", err)
	}
}

func TestOpenRecordingAudioTamperFailsWithoutSilentBytes(t *testing.T) {
	f := newRecordingFixture(t)
	row := f.writeRecording(t, "c-alice", "rec_tamper", make([]byte, 3000))
	path := filepath.Join(f.dataDir, row.StoragePath)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[recording.HeaderSize+5] ^= 0xFF
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	open, err := f.svc.OpenRecordingAudio(context.Background(), aliceActor(), "c-alice", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = open.Close() }()
	_, err = io.ReadAll(open.Audio)
	if err == nil {
		t.Fatal("tampered segment read without error")
	}
	if got := open.Audio.(*wavStream).Failed(); got == nil {
		t.Fatal("stream did not record the failure for the HTTP layer")
	}
}

func TestOpenRecordingAudioForeignAndMissingAreIdentical(t *testing.T) {
	f := newRecordingFixture(t)
	f.writeRecording(t, "c-alice", "rec_one", make([]byte, 40))
	_, foreign := f.svc.OpenRecordingAudio(context.Background(), Actor{UserID: "bob", Scopes: []string{ScopeRead}}, "c-alice", "")
	_, missing := f.svc.OpenRecordingAudio(context.Background(), aliceActor(), "c-nope", "")
	if AsError(foreign).Code != CodeNotFound || AsError(missing).Code != CodeNotFound {
		t.Fatalf("codes differ: %v / %v", foreign, missing)
	}
	if AsError(foreign).Message != AsError(missing).Message {
		t.Fatal("foreign and missing messages differ")
	}
}

func TestAuditRangeDetailTruncatesAndEncodesSafely(t *testing.T) {
	long := strings.Repeat("a", 200)
	out := auditRangeDetail(long)
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("detail is not JSON: %v (%s)", err, out)
	}
	if len(m["range"]) != maxAuditRangeLen {
		t.Fatalf("range length = %d, want %d", len(m["range"]), maxAuditRangeLen)
	}
	hostile := "bytes=\"\n\\x"
	if err := json.Unmarshal([]byte(auditRangeDetail(hostile)), &m); err != nil {
		t.Fatalf("hostile range produced invalid JSON: %v", err)
	}
	if auditRangeDetail("") != "{}" {
		t.Fatal("absent Range must be an empty object")
	}
}

func TestRecordingViewOfRejectsMalformedTimestamp(t *testing.T) {
	_, err := recordingViewOf(storage.Recording{CreatedAt: "not a time"})
	if errorCodeOf(t, err) != CodeInternal {
		t.Fatalf("malformed timestamp err = %v, want internal", err)
	}
}

func TestRecordingAvailableRule(t *testing.T) {
	cases := []struct {
		status string
		bytes  int64
		want   bool
	}{
		{storage.RecordingStatusComplete, 0, true},
		{storage.RecordingStatusPartial, 1, true},
		{storage.RecordingStatusPartial, 0, false},
		{storage.RecordingStatusRecording, 0, false},
		{storage.RecordingStatusRecording, 512, false},
		{storage.RecordingStatusFailed, 512, false},
		{storage.RecordingStatusDeleted, 512, false},
	}
	for _, tc := range cases {
		if got := recordingAvailable(tc.status, tc.bytes); got != tc.want {
			t.Fatalf("recordingAvailable(%q, %d) = %v, want %v", tc.status, tc.bytes, got, tc.want)
		}
	}
}
