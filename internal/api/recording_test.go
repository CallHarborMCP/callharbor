package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/recording"
)

// writeAliceRecording writes a finalized encrypted recording for alice-call
// with the given PCM, using the real writer, and inserts its row through the
// production store. It returns the PCM so tests can compare bytes.
func writeAliceRecording(t *testing.T, f *fixture, id string, pcm []byte) []byte {
	t.Helper()
	ctx := context.Background()
	h := recording.Header{KeyVersion: 1, SampleRate: 8000}
	for i := range h.Salt {
		h.Salt[i] = byte(0x40 + i)
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
	if _, err := f.raw.ExecContext(ctx, `INSERT INTO recordings
		(id, call_id, owner_user_id, status, storage_path, byte_count, duration_ms,
		 key_version, created_at, finalized_at, sample_rate, channels, quality_flags)
		VALUES (?, 'alice-call', 'alice', 'complete', ?, ?, 1000, 1, ?, ?, 8000, 2, '["agent_playout_estimated"]')`,
		id, recording.RelPath("alice", id), len(pcm), seedTime, seedTime); err != nil {
		t.Fatalf("insert recording row: %v", err)
	}
	return pcm
}

func audioPath(callID string) string { return "/api/v1/calls/" + callID + "/recording/audio" }

func TestV1RecordingMetadataIsOwnerScopedAndValid(t *testing.T) {
	f := newFixture(t)
	writeAliceRecording(t, f, "rec_meta", make([]byte, 400))

	rec := get(t, f.mux, "/api/v1/calls/alice-call/recording", f.aliceCk)
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata = %d %s", rec.Code, rec.Body.String())
	}
	validateBody(t, compileComponent(t, "RecordingMetadata"), rec)
	m := decodeMap(t, rec)
	if m["status"] != "complete" || m["byte_count"] != float64(444) || m["partial"] != false {
		t.Fatalf("metadata = %v", m)
	}
	if m["download_path"] != audioPath("alice-call") {
		t.Fatalf("download_path = %v", m["download_path"])
	}
	if strings.Contains(rec.Body.String(), f.dataDir) || strings.Contains(rec.Body.String(), "storage_path") {
		t.Fatalf("metadata leaks a storage path: %s", rec.Body.String())
	}
}

func TestV1RecordingForeignAndMissingAreIdenticalNotFound(t *testing.T) {
	f := newFixture(t)
	writeAliceRecording(t, f, "rec_foreign", make([]byte, 40))
	cases := []struct {
		path string
		ck   bool // true: bob (foreign), false: alice asking for a missing call
	}{
		{"/api/v1/calls/alice-call/recording", true},
		{"/api/v1/calls/alice-call/recording/audio", true},
		{"/api/v1/calls/missing/recording", false},
		{"/api/v1/calls/missing/recording/audio", false},
	}
	var bodies []string
	for _, c := range cases {
		ck := f.aliceCk
		if c.ck {
			ck = f.bobCk
		}
		rec := get(t, f.mux, c.path, ck)
		if rec.Code != http.StatusNotFound || errCode(t, rec) != "not_found" {
			t.Fatalf("%s = %d %s", c.path, rec.Code, rec.Body.String())
		}
		validateBody(t, compileComponent(t, "Error"), rec)
		bodies = append(bodies, rec.Body.String())
	}
	if bodies[0] != bodies[2] || bodies[1] != bodies[3] {
		t.Fatalf("foreign and missing bodies differ:\n%v", bodies)
	}
}

func TestV1RecordingRequiresSession(t *testing.T) {
	f := newFixture(t)
	writeAliceRecording(t, f, "rec_anon", make([]byte, 40))
	for _, p := range []string{"/api/v1/calls/alice-call/recording", audioPath("alice-call")} {
		rec := get(t, f.mux, p, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d", p, rec.Code)
		}
	}
}

func TestV1RecordingAudioFullBodyIsHeaderPlusPCM(t *testing.T) {
	f := newFixture(t)
	pcm := make([]byte, 2*recording.SegmentSize+256)
	for i := range pcm {
		pcm[i] = byte(i * 13)
	}
	writeAliceRecording(t, f, "rec_full", pcm)

	rec := get(t, f.mux, audioPath("alice-call"), f.aliceCk)
	if rec.Code != http.StatusOK {
		t.Fatalf("audio = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
		t.Fatalf("content-type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("cache-control = %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Fatalf("disposition = %q, want inline", got)
	}
	body := rec.Body.Bytes()
	if len(body) != 44+len(pcm) {
		t.Fatalf("body = %d bytes, want %d", len(body), 44+len(pcm))
	}
	if string(body[:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		t.Fatalf("not a WAV: %q", body[:12])
	}
	if !bytes.Equal(body[44:], pcm) {
		t.Fatal("PCM does not match the recorded bytes")
	}
}

func TestV1RecordingAudioRangeSpansSegmentBoundary(t *testing.T) {
	f := newFixture(t)
	pcm := make([]byte, 2*recording.SegmentSize+512)
	for i := range pcm {
		pcm[i] = byte(i % 253)
	}
	writeAliceRecording(t, f, "rec_range", pcm)

	// The window starts 10 bytes before the first segment boundary of the PCM
	// and runs 30 bytes past it.
	start := 44 + recording.SegmentSize - 10
	end := start + 39
	req := httptest.NewRequest(http.MethodGet, audioPath("alice-call"), nil)
	req.AddCookie(f.aliceCk)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range = %d %s", rec.Code, rec.Body.String())
	}
	wantCR := fmt.Sprintf("bytes %d-%d/%d", start, end, 44+len(pcm))
	if got := rec.Header().Get("Content-Range"); got != wantCR {
		t.Fatalf("content-range = %q, want %q", got, wantCR)
	}
	want := pcm[recording.SegmentSize-10 : recording.SegmentSize+30]
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatal("range bytes across the segment boundary are wrong")
	}
}

func TestV1RecordingAudioUnsatisfiableRangeIs416(t *testing.T) {
	f := newFixture(t)
	pcm := make([]byte, 100)
	writeAliceRecording(t, f, "rec_416", pcm)
	req := httptest.NewRequest(http.MethodGet, audioPath("alice-call"), nil)
	req.AddCookie(f.aliceCk)
	req.Header.Set("Range", "bytes=99999-")
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("416 case = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes */%d", 44+len(pcm)) {
		t.Fatalf("content-range = %q", got)
	}
}

func TestV1RecordingAudioDownloadDisposition(t *testing.T) {
	f := newFixture(t)
	writeAliceRecording(t, f, "rec_dl", make([]byte, 40))
	rec := get(t, f.mux, audioPath("alice-call")+"?download=1", f.aliceCk)
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="call-alice-call.wav"` {
		t.Fatalf("disposition = %q", got)
	}
}

func TestV1RecordingCallDownloadNameIsSanitized(t *testing.T) {
	cases := map[string]string{
		"alice-call":    "call-alice-call.wav",
		`a"b;c d`:       "call-a_b_c_d.wav",
		"../../etc/pwd": "call-______etc_pwd.wav",
	}
	for in, want := range cases {
		if got := callDownloadName(in); got != want {
			t.Fatalf("callDownloadName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestV1RecordingAudioInProgressIsConflict(t *testing.T) {
	f := newFixture(t)
	if _, err := f.raw.ExecContext(context.Background(), `INSERT INTO recordings
		(id, call_id, owner_user_id, status, storage_path, byte_count, duration_ms, key_version, created_at)
		VALUES ('rec_busy', 'alice-call', 'alice', 'recording', ?, 0, 0, 1, ?)`,
		recording.RelPath("alice", "rec_busy"), seedTime); err != nil {
		t.Fatal(err)
	}
	rec := get(t, f.mux, audioPath("alice-call"), f.aliceCk)
	if rec.Code != http.StatusConflict || errCode(t, rec) != "conflict" {
		t.Fatalf("in-progress audio = %d %s", rec.Code, rec.Body.String())
	}
	validateBody(t, compileComponent(t, "Error"), rec)
	meta := get(t, f.mux, "/api/v1/calls/alice-call/recording", f.aliceCk)
	if meta.Code != http.StatusOK || decodeMap(t, meta)["status"] != "recording" {
		t.Fatalf("in-progress metadata = %d %s", meta.Code, meta.Body.String())
	}
}

func TestV1RecordingNoRowIsNotFoundForAudio(t *testing.T) {
	f := newFixture(t)
	rec := get(t, f.mux, audioPath("alice-call"), f.aliceCk)
	if rec.Code != http.StatusNotFound || errCode(t, rec) != "not_found" {
		t.Fatalf("no-recording audio = %d %s", rec.Code, rec.Body.String())
	}
}

func TestV1RecordingAuditFailureReturnsNoBody(t *testing.T) {
	f := newFixture(t)
	writeAliceRecording(t, f, "rec_audit", make([]byte, 40))
	if _, err := f.raw.ExecContext(context.Background(), `DROP TABLE audit_events`); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/api/v1/calls/alice-call/recording", audioPath("alice-call")} {
		rec := get(t, f.mux, p, f.aliceCk)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s with audit failure = %d, want 500", p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "RIFF") || strings.Contains(rec.Body.String(), "call_id") {
			t.Fatalf("%s leaked a body on audit failure: %q", p, rec.Body.String())
		}
	}
}

func TestV1RecordingAuditRecordsReadsAndRefusals(t *testing.T) {
	f := newFixture(t)
	writeAliceRecording(t, f, "rec_audit2", make([]byte, 40))
	get(t, f.mux, "/api/v1/calls/alice-call/recording", f.aliceCk)
	get(t, f.mux, audioPath("alice-call"), f.aliceCk)
	req := httptest.NewRequest(http.MethodGet, audioPath("alice-call"), nil)
	req.AddCookie(f.aliceCk)
	req.Header.Set("Range", "bytes=0-9")
	f.mux.ServeHTTP(httptest.NewRecorder(), req)
	get(t, f.mux, audioPath("alice-call"), f.bobCk)

	rows, err := f.raw.QueryContext(context.Background(), `SELECT action, outcome, COALESCE(detail_json, '') FROM audit_events WHERE action LIKE 'recording.%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var action, outcome, detail string
		if err := rows.Scan(&action, &outcome, &detail); err != nil {
			t.Fatal(err)
		}
		got = append(got, action+"/"+outcome)
		if strings.Contains(detail, f.dataDir) {
			t.Fatalf("audit detail names a path: %s", detail)
		}
	}
	want := []string{
		"recording.read/success",
		"recording.audio_read/success",
		"recording.audio_read/success",
		"recording.audio_read/denied",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit rows = %v, want %v", got, want)
	}
}
