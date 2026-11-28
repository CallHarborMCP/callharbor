package callstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voicecall"
)

// clearStore is a Store for the sink factory. Its call is live and bound, and
// the owner has opted out since acceptance. clearErr makes the clear fail.
type clearStore struct {
	call     storage.MediaCall
	clears   int
	clearErr error
}

func (s *clearStore) LoadCallForMedia(context.Context, string) (storage.MediaCall, error) {
	return s.call, nil
}

func (s *clearStore) BeginLocalHangup(context.Context, string, string, time.Time) (storage.LiveCallLeg, bool, error) {
	return storage.LiveCallLeg{}, false, nil
}

func (s *clearStore) BindStreamControlID(context.Context, string, string, time.Time) (bool, error) {
	return true, nil
}

func (s *clearStore) ClearRecordEffective(context.Context, string) (bool, error) {
	s.clears++
	return false, s.clearErr
}

// failingProfiles makes the resolver fail, so buildSink stops before any AI
// dial. The clear runs before buildSink, so its behavior is what is tested.
type failingProfiles struct{}

func (failingProfiles) GetAIProfile(context.Context, string, string) (storage.AIProfile, error) {
	return storage.AIProfile{}, errors.New("profile lookup stopped by test")
}

// secretNone is never reached, since the profile lookup fails first.
type secretNone struct{}

func (secretNone) Resolve(context.Context, string, string, string) ([]byte, error) {
	return nil, errors.New("unused")
}

// newClearMedia builds a Media whose factory stops at the resolver. The stream
// start frame names the call the way the dial does, base64 client_state.
func newClearMedia(t *testing.T, store *clearStore, logger *slog.Logger) (*Media, telnyx.Frame) {
	t.Helper()
	res, err := voicecall.NewResolver(failingProfiles{}, secretNone{}, audio.Format{Encoding: audio.L16, SampleRate: 8000, LittleEndian: true}, realtime.EndpointPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	m := &Media{Store: store, Resolver: res, Now: time.Now, Logger: logger, Hangup: nil}
	start := telnyx.Frame{
		CallControlID: "ctl-1",
		ClientState:   base64.StdEncoding.EncodeToString([]byte("call_optout")),
	}
	return m, start
}

// The factory clears record_effective after the stream binds, when the opt-out
// came after the call was accepted.
func TestSinkFactoryClearsRecordEffectiveAfterOptOut(t *testing.T) {
	store := &clearStore{call: storage.MediaCall{ID: "call_optout", Status: "active", ControlID: "ctl-1"}}
	m, start := newClearMedia(t, store, nil)

	_, _ = m.sinkFactory(storage.MediaCall{ID: "call_optout"})(context.Background(), start, nil)
	if store.clears != 1 {
		t.Fatalf("ClearRecordEffective calls = %d, want 1", store.clears)
	}
}

// A failed clear is logged and the stream continues past it. It is not refused:
// the failure reaches the logger, and the factory proceeds to the resolver.
func TestSinkFactoryClearFailureIsLoggedNotRefused(t *testing.T) {
	store := &clearStore{
		call:     storage.MediaCall{ID: "call_optout", Status: "active", ControlID: "ctl-1"},
		clearErr: errors.New("disk busy"),
	}
	var buf bytes.Buffer
	m, start := newClearMedia(t, store, slog.New(slog.NewTextHandler(&buf, nil)))

	_, err := m.sinkFactory(storage.MediaCall{ID: "call_optout"})(context.Background(), start, nil)
	if err == nil || strings.Contains(err.Error(), "clear record effective") {
		t.Fatalf("err = %v, want the later resolver failure, not the clear", err)
	}
	if !strings.Contains(buf.String(), "could not clear record_effective") {
		t.Fatalf("clear failure not logged: %q", buf.String())
	}
}
