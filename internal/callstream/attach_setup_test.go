package callstream

import (
	"context"
	"errors"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/transcript"
)

// failingTranscriptStore fails the transcript recorder's setup read, so
// transcript.New returns an error after every step before it has run.
type failingTranscriptStore struct{}

var errTranscriptSetup = errors.New("transcript store unavailable")

func (failingTranscriptStore) RecordTranscript(context.Context, string, string, []storage.TranscriptRevision, []storage.CallEvent) error {
	return nil
}

func (failingTranscriptStore) MaxTranscriptSeq(context.Context, string, string) (int, error) {
	return 0, errTranscriptSetup
}

var _ transcript.Store = failingTranscriptStore{}

// A transcript setup failure after the recording's setup would have started
// must not leave a live recording. attach returns the error, releases the
// claim, and never inserts a recording row. The row is created only after
// every fallible step, so the call leaves no row in 'recording'.
func TestAttachTranscriptSetupFailureLeavesNoRecordingRow(t *testing.T) {
	store := &startFakeStore{}
	m := &Media{
		Transcripts: failingTranscriptStore{},
		Recording:   startCfg(t, store),
	}
	sess := newFakeSession()
	release, err := m.sessions.claim("alice-call")
	if err != nil {
		t.Fatal(err)
	}
	leg := audio.Format{Encoding: audio.L16, SampleRate: 8000, LittleEndian: true}
	provider := audio.Format{Encoding: audio.L16, SampleRate: 8000, LittleEndian: true}
	st := startCall()
	st.ID = "alice-call"
	send := func(context.Context, []byte) error { return nil }
	_, err = m.attach(sess, st, leg, provider, realtime.Capabilities{}, send, release)
	if !errors.Is(err, errTranscriptSetup) {
		t.Fatalf("attach error = %v, want the transcript setup error", err)
	}
	if len(store.inserted) != 0 {
		t.Fatalf("recording rows inserted = %d, want 0 after a later setup failure", len(store.inserted))
	}
	if len(store.finalized) != 0 {
		t.Fatalf("recording finalized after a refused attach: %+v", store.finalized)
	}
	select {
	case <-sess.closed:
	default:
		t.Fatal("refused attach did not close the provider session")
	}
	if _, err := m.sessions.claim("alice-call"); err != nil {
		t.Fatalf("claim after refused attach = %v, want the call's slot free", err)
	}
}
