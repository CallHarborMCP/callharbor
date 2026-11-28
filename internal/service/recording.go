package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/CallHarborMCP/callharbor/internal/recording"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// Audit vocabulary for recording reads.
const (
	auditRecordingRead      = "recording.read"
	auditRecordingAudioRead = "recording.audio_read"
	// maxAuditRangeLen caps the Range header value written to the audit detail.
	maxAuditRangeLen = 64
)

// Fixed metadata values the recording view exposes. The channel layout is part
// of the contract: channel 0 is the remote party, channel 1 the agent.
const (
	recordingContentType = "audio/wav"
	recordingChannels    = 2
)

var recordingChannelLayout = []string{"remote_party", "agent"}

// recordingDownloadPath is the authenticated REST path of a call's audio.
func recordingDownloadPath(callID string) string {
	return "/api/v1/calls/" + callID + "/recording/audio"
}

// RecordingStore is the storage the recording read path needs.
type RecordingStore interface {
	GetRecordingForCall(ctx context.Context, ownerUserID, callID string) (storage.Recording, error)
	InsertAudit(ctx context.Context, e storage.AuditEvent) error
}

// recordingDeps is what WithRecordings attaches. ring is nil when no master key
// is configured, and then the audio path is unavailable.
type recordingDeps struct {
	store   RecordingStore
	ring    *secrets.Keyring
	dataDir string
}

// WithRecordings attaches the recording read path. ring is the master keyring
// (nil when no key is configured) and dataDir the root of the recordings tree.
// Until it is set, the recording reads return unavailable without touching the
// store.
func (s *Service) WithRecordings(store RecordingStore, ring *secrets.Keyring, dataDir string) *Service {
	s.recordings = &recordingDeps{store: store, ring: ring, dataDir: dataDir}
	return s
}

// RecordingView is one recording's metadata as the API and MCP expose it. It
// never carries a file path or key material.
type RecordingView struct {
	CallID        string   `json:"call_id"`
	Status        string   `json:"status"`
	ContentType   string   `json:"content_type"`
	DurationMS    int64    `json:"duration_ms"`
	ByteCount     int64    `json:"byte_count"`
	DownloadPath  string   `json:"download_path"`
	SampleRate    int      `json:"sample_rate"`
	Channels      int      `json:"channels"`
	ChannelLayout []string `json:"channel_layout"`
	QualityFlags  []string `json:"quality_flags"`
	Partial       bool     `json:"partial"`
	CreatedAt     string   `json:"created_at"`
	FinalizedAt   *string  `json:"finalized_at"`
}

// RecordingAudio is an open recording for one audio read. Audio is the WAV file
// as a seekable stream: the 44-byte header followed by decrypted PCM. The
// caller must Close it.
type RecordingAudio struct {
	View  RecordingView
	Audio io.ReadSeeker
	close func() error
}

// Close releases the recording file.
func (a *RecordingAudio) Close() error {
	if a == nil || a.close == nil {
		return nil
	}
	return a.close()
}

// GetRecording returns the metadata of one call's recording. It is owner-scoped:
// a foreign call and a missing call get the same not_found, so existence is not
// revealed. Every read, served or refused, writes a recording.read audit row.
func (s *Service) GetRecording(ctx context.Context, actor Actor, callID string) (RecordingView, error) {
	if err := actor.Require(ScopeRead); err != nil {
		return RecordingView{}, err
	}
	if s.recordings == nil {
		return RecordingView{}, AsErrorWith(CodeUnavailable, "recordings are not available")
	}
	if callID == "" || len(callID) > MaxIDLength {
		return RecordingView{}, invalidInput("call_id must be 1 to %d characters", MaxIDLength)
	}
	rec, err := s.lookupRecording(ctx, actor, callID, auditRecordingRead, "{}")
	if err != nil {
		return RecordingView{}, err
	}
	view, err := recordingViewOf(rec)
	if err != nil {
		return RecordingView{}, err
	}
	if err := s.auditRecording(ctx, actor, callID, auditRecordingRead, auditOutcomeSuccess, "{}"); err != nil {
		return RecordingView{}, err
	}
	return view, nil
}

// OpenRecordingAudio opens one call's recording for reading. The status rules
// are the same as GetRecording's, plus: a recording in progress is a conflict,
// and a finalized one with no audio is not available. rangeHeader is the raw
// Range header of the request, recorded in the audit detail (truncated); pass
// "" when there is none. A refusal is audited, and so is a success. The caller
// closes the returned audio.
func (s *Service) OpenRecordingAudio(ctx context.Context, actor Actor, callID, rangeHeader string) (RecordingAudio, error) {
	if err := actor.Require(ScopeRead); err != nil {
		return RecordingAudio{}, err
	}
	if s.recordings == nil {
		return RecordingAudio{}, AsErrorWith(CodeUnavailable, "recordings are not available")
	}
	if callID == "" || len(callID) > MaxIDLength {
		return RecordingAudio{}, invalidInput("call_id must be 1 to %d characters", MaxIDLength)
	}
	detail := auditRangeDetail(rangeHeader)
	rec, err := s.lookupRecording(ctx, actor, callID, auditRecordingAudioRead, detail)
	if err != nil {
		return RecordingAudio{}, err
	}
	if rec.Status == storage.RecordingStatusRecording {
		if err := s.auditRecording(ctx, actor, callID, auditRecordingAudioRead, auditOutcomeDenied, auditDetailWithReason(detail, "in_progress")); err != nil {
			return RecordingAudio{}, err
		}
		return RecordingAudio{}, newError(CodeConflict, "recording in progress")
	}
	f, pcm, err := s.openRecordingFile(rec)
	if err != nil {
		if auditErr := s.auditRecording(ctx, actor, callID, auditRecordingAudioRead, auditOutcomeDenied, auditDetailWithReason(detail, "unreadable")); auditErr != nil {
			return RecordingAudio{}, auditErr
		}
		return RecordingAudio{}, err
	}
	header, err := recording.WAVHeader(rec.SampleRate, pcm.PlainLen())
	if err != nil {
		_ = f.Close()
		return RecordingAudio{}, internal(fmt.Errorf("wav header: %w", err))
	}
	view, err := recordingViewOf(rec)
	if err != nil {
		_ = f.Close()
		return RecordingAudio{}, err
	}
	if err := s.auditRecording(ctx, actor, callID, auditRecordingAudioRead, auditOutcomeSuccess, detail); err != nil {
		_ = f.Close()
		return RecordingAudio{}, err
	}
	return RecordingAudio{
		View:  view,
		Audio: newWAVStream(header, pcm),
		close: f.Close,
	}, nil
}

// lookupRecording resolves the call and its recording for the actor. A foreign
// or missing call, a missing recording row, a failed or deleted recording, and a
// finalized recording with no audio all return not_found with a fixed message:
// the first two are the same answer, so a caller cannot tell a foreign call from
// a missing one. Each refusal is audited under action with a reason code. An
// in-progress recording is returned as is; the audio path decides what it means.
func (s *Service) lookupRecording(ctx context.Context, actor Actor, callID, action, detail string) (storage.Recording, error) {
	if _, err := s.store.GetCall(ctx, actor.UserID, callID); errors.Is(err, storage.ErrCallNotFound) {
		return storage.Recording{}, s.refuseRecording(ctx, actor, callID, action, detail, "call_not_found", "call not found")
	} else if err != nil {
		return storage.Recording{}, internal(fmt.Errorf("get call: %w", err))
	}
	rec, err := s.recordings.store.GetRecordingForCall(ctx, actor.UserID, callID)
	if errors.Is(err, storage.ErrRecordingNotFound) {
		return storage.Recording{}, s.refuseRecording(ctx, actor, callID, action, detail, "no_recording", "recording not available")
	}
	if err != nil {
		return storage.Recording{}, internal(fmt.Errorf("get recording: %w", err))
	}
	switch rec.Status {
	case storage.RecordingStatusRecording:
		return rec, nil
	case storage.RecordingStatusComplete, storage.RecordingStatusPartial:
		if rec.ByteCount == 0 {
			return storage.Recording{}, s.refuseRecording(ctx, actor, callID, action, detail, "no_audio", "recording not available")
		}
		return rec, nil
	default:
		return storage.Recording{}, s.refuseRecording(ctx, actor, callID, action, detail, "not_servable", "recording not available")
	}
}

// refuseRecording audits a denied or unavailable read and returns the not_found
// error for it. The audit reason is a fixed code; it never names a call's owner
// or a path.
func (s *Service) refuseRecording(ctx context.Context, actor Actor, callID, action, detail, reason, message string) error {
	if err := s.auditRecording(ctx, actor, callID, action, auditOutcomeDenied, auditDetailWithReason(detail, reason)); err != nil {
		return err
	}
	return newError(CodeNotFound, message)
}

// openRecordingFile opens the .chrec file, authenticates its header against the
// row, and returns a reader over the plaintext. It fails closed when no master
// key is configured.
func (s *Service) openRecordingFile(rec storage.Recording) (*os.File, *recording.Reader, error) {
	ring := s.recordings.ring
	if ring == nil {
		return nil, nil, AsErrorWith(CodeUnavailable, "recordings are not available: no master key is configured")
	}
	path, err := recordingFilePath(s.recordings.dataDir, rec)
	if err != nil {
		return nil, nil, internal(err)
	}
	f, err := os.Open(path) // #nosec G304 -- path is built by recording.Path from validated ids and matched to StoragePath
	if err != nil {
		return nil, nil, internal(fmt.Errorf("open recording: %w", err))
	}
	fail := func(err error) (*os.File, *recording.Reader, error) {
		_ = f.Close()
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return fail(internal(fmt.Errorf("stat recording: %w", err)))
	}
	headerBytes := make([]byte, recording.HeaderSize)
	if _, err := io.ReadFull(f, headerBytes); err != nil {
		return fail(internal(fmt.Errorf("read recording header: %w", err)))
	}
	h, err := recording.DecodeHeader(headerBytes)
	if err != nil {
		return fail(internal(fmt.Errorf("recording header: %w", err)))
	}
	if h.KeyVersion != rec.KeyVersion || h.SampleRate != rec.SampleRate {
		return fail(internal(errors.New("recording header does not match its row")))
	}
	key, err := ring.DeriveRecordingKey(h.KeyVersion, h.Salt[:])
	if err != nil {
		return fail(internal(fmt.Errorf("derive recording key: %w", err)))
	}
	reader, err := recording.NewReader(f, st.Size(), headerBytes, h, key, rec.ID, rec.OwnerUserID, rec.ByteCount)
	if err != nil {
		return fail(internal(fmt.Errorf("open recording reader: %w", err)))
	}
	return f, reader, nil
}

// recordingFilePath is the absolute path of rec's file under dataDir. The stored
// relative path is checked against the one the id and owner derive, so a row
// cannot point the reader elsewhere.
func recordingFilePath(dataDir string, rec storage.Recording) (string, error) {
	want := recording.RelPath(rec.OwnerUserID, rec.ID)
	if rec.StoragePath != want {
		return "", fmt.Errorf("recording path does not match its id")
	}
	path, err := recording.Path(dataDir, rec.OwnerUserID, rec.ID)
	if err != nil {
		return "", err
	}
	return path, nil
}

// recordingViewOf builds the metadata view of a stored recording. byte_count is
// the size of the WAV the audio endpoint serves: the 44-byte header plus the PCM
// bytes. An in-progress recording has no audio yet, so its count is 0. Stored
// timestamps are formatted as the call views format theirs.
func recordingViewOf(rec storage.Recording) (RecordingView, error) {
	created, err := formatTimestamp(rec.CreatedAt)
	if err != nil {
		return RecordingView{}, internal(err)
	}
	v := RecordingView{
		CallID:        rec.CallID,
		Status:        rec.Status,
		ContentType:   recordingContentType,
		DurationMS:    rec.DurationMS,
		DownloadPath:  recordingDownloadPath(rec.CallID),
		SampleRate:    rec.SampleRate,
		Channels:      recordingChannels,
		ChannelLayout: append([]string(nil), recordingChannelLayout...),
		QualityFlags:  append([]string{}, rec.QualityFlags...),
		Partial:       rec.Status != storage.RecordingStatusComplete,
		CreatedAt:     created,
	}
	if rec.ByteCount > 0 {
		v.ByteCount = recording.WAVHeaderSize + rec.ByteCount
	}
	if rec.FinalizedAt != "" {
		fin, err := formatTimestamp(rec.FinalizedAt)
		if err != nil {
			return RecordingView{}, internal(err)
		}
		v.FinalizedAt = &fin
	}
	return v, nil
}

// auditRecording writes one recording read. A failed write returns internal so
// the read never passes unrecorded.
func (s *Service) auditRecording(ctx context.Context, actor Actor, callID, action, outcome, detail string) error {
	err := s.recordings.store.InsertAudit(ctx, storage.AuditEvent{
		OccurredAt:   s.now().UTC().Format(storageTimeLayout),
		ActorUserID:  actor.UserID,
		ActorRole:    auditRoleUser,
		TargetUserID: actor.UserID,
		Action:       action,
		ResourceType: auditResourceCall,
		ResourceID:   callID,
		Outcome:      outcome,
		DetailJSON:   detail,
	})
	if err != nil {
		return internal(fmt.Errorf("audit %s: %w", action, err))
	}
	return nil
}

// auditRangeDetail is the audit detail for a read: the Range header value,
// truncated, as a JSON object. An absent Range is an empty object. The value is
// quoted by encoding/json, so any header bytes produce valid JSON.
func auditRangeDetail(rangeHeader string) string {
	if rangeHeader == "" {
		return "{}"
	}
	v := rangeHeader
	if len(v) > maxAuditRangeLen {
		v = v[:maxAuditRangeLen]
	}
	b, _ := json.Marshal(map[string]string{"range": v})
	return string(b)
}

// auditDetailWithReason adds a fixed reason code to an audit detail object.
func auditDetailWithReason(detail, reason string) string {
	m := map[string]string{}
	if detail != "" && detail != "{}" {
		_ = json.Unmarshal([]byte(detail), &m)
	}
	m["reason"] = reason
	b, _ := json.Marshal(m)
	return string(b)
}

// wavStream is the WAV file as one seekable stream: the 44-byte header, then the
// decrypted PCM. http.ServeContent serves ranges over it without knowing the
// file is encrypted. Every PCM read goes through the reader, which authenticates
// each segment before returning its bytes.
type wavStream struct {
	header []byte
	pcm    *recording.Reader
	pos    int64
	size   int64
	// failed is the first PCM read error. Once set, every read fails with it,
	// so a transport that ignores the first error cannot read past it.
	failed error
}

func newWAVStream(header []byte, pcm *recording.Reader) *wavStream {
	return &wavStream{header: header, pcm: pcm, size: int64(len(header)) + pcm.PlainLen()}
}

// Failed returns the decryption or I/O error that ended the stream early, or nil.
// The HTTP layer checks it after the copy, because ServeContent has no error
// return and a short copy would otherwise look like a finished response.
func (w *wavStream) Failed() error { return w.failed }

// Read fills p from the header and then the PCM. A decryption failure is
// returned as an error with the bytes read so far, so the HTTP layer can abort
// the response instead of sending data that did not authenticate.
func (w *wavStream) Read(p []byte) (int, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	if w.pos >= w.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	total := 0
	hl := int64(len(w.header))
	if w.pos < hl {
		n := copy(p, w.header[w.pos:])
		total += n
		w.pos += int64(n)
		p = p[n:]
	}
	if len(p) > 0 && w.pos >= hl && w.pos < w.size {
		if _, err := w.pcm.Seek(w.pos-hl, io.SeekStart); err != nil {
			w.failed = err
			return total, err
		}
		n, err := w.pcm.Read(p)
		total += n
		w.pos += int64(n)
		if err != nil && !errors.Is(err, io.EOF) {
			w.failed = err
			return total, err
		}
	}
	return total, nil
}

func (w *wavStream) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = w.pos + offset
	case io.SeekEnd:
		abs = w.size + offset
	default:
		return 0, errors.New("service: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("service: negative seek")
	}
	w.pos = abs
	return abs, nil
}
