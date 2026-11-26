package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// memStore keeps every revision and event the recorder writes, in order.
type memStore struct {
	mu     sync.Mutex
	revs   []storage.TranscriptRevision
	events []storage.CallEvent
	fail   error
}

func (s *memStore) RecordTranscript(_ context.Context, callID, owner string, turns []storage.TranscriptRevision, events []storage.CallEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	// Mirror the transcript_turns CHECK on correlation_id, so an oversized id
	// fails the write here as it would in SQLite.
	for _, r := range turns {
		if n := len(r.CorrelationID); r.CorrelationID != "" && (n < 1 || n > 128) {
			return errors.New("CHECK constraint failed: correlation_id")
		}
	}
	s.revs = append(s.revs, turns...)
	s.events = append(s.events, events...)
	return nil
}

// latest returns the last revision written for each seq, in seq order.
func (s *memStore) latest() []storage.TranscriptRevision {
	s.mu.Lock()
	defer s.mu.Unlock()
	by := map[int]storage.TranscriptRevision{}
	for _, r := range s.revs {
		by[r.Seq] = r
	}
	out := make([]storage.TranscriptRevision, 0, len(by))
	for i := 1; i <= len(by); i++ {
		out = append(out, by[i])
	}
	return out
}

func (s *memStore) revisionsOf(seq int) []storage.TranscriptRevision {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storage.TranscriptRevision
	for _, r := range s.revs {
		if r.Seq == seq {
			out = append(out, r)
		}
	}
	return out
}

// MaxTranscriptSeq reports the highest seq written so far, as storage does.
func (s *memStore) MaxTranscriptSeq(_ context.Context, _, _ string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	maxSeq := 0
	for _, r := range s.revs {
		maxSeq = max(maxSeq, r.Seq)
	}
	return maxSeq, nil
}

func (s *memStore) summary(t *testing.T) (flags []string, complete bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Kind != storage.TranscriptFinalizedKind {
			continue
		}
		var p struct {
			Flags    []string `json:"quality_flags"`
			Complete bool     `json:"two_party_complete"`
		}
		if err := json.Unmarshal([]byte(e.PayloadJSON), &p); err != nil {
			t.Fatal(err)
		}
		return p.Flags, p.Complete
	}
	t.Fatal("no transcript.finalized event")
	return nil, false
}

func (s *memStore) eventKinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		out = append(out, e.Kind)
	}
	return out
}

// harness is a recorder with scripted playout accounting and a manual clock.
type harness struct {
	rec     *Recorder
	store   *memStore
	mu      sync.Mutex
	playout map[audio.SourceRef]audio.ItemPlayout
	utt     uint64
	now     time.Time
}

func newHarness(t *testing.T, caps realtime.Capabilities) *harness {
	t.Helper()
	h := &harness{store: &memStore{}, playout: map[audio.SourceRef]audio.ItemPlayout{}, utt: 1, now: time.Unix(1_800_000_000, 0)}
	rec, err := New(Config{
		CallID: "call_1", OwnerUserID: "alice", Caps: caps, Store: h.store,
		Playout: func(src audio.SourceRef) (audio.ItemPlayout, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			p, ok := h.playout[src]
			return p, ok
		},
		SourceOf: func(ev realtime.Event) audio.SourceRef {
			if ev.ItemID != "" {
				return audio.SourceRef{ItemID: ev.ItemID, ContentIndex: ev.ContentIndex}
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			return audio.SourceRef{Utterance: h.utt}
		},
		Now: func() time.Time {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.now
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.rec = rec
	return h
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *harness) setPlayout(src audio.SourceRef, p audio.ItemPlayout) {
	h.mu.Lock()
	h.playout[src] = p
	h.mu.Unlock()
}

func (h *harness) nextUtterance() {
	h.mu.Lock()
	h.utt++
	h.mu.Unlock()
}

func (h *harness) close(t *testing.T) {
	t.Helper()
	if err := h.rec.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

const ms = time.Millisecond

func played(d time.Duration) audio.ItemPlayout {
	return audio.ItemPlayout{Sent: d, Confirmed: d, Estimated: d}
}

func hasFlag(flags []string, f string) bool { return slices.Contains(flags, f) }

// Realtime: agent text comes from the output audio transcript keyed by item_id
// and is stored verbatim; caller speech is an explicit turn with no text,
// because V1 never enables billed input transcription.
func TestRealtimeAgentTextAndUnavailableCaller(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	src := audio.SourceRef{ItemID: "item_a"}
	h.rec.Observe(realtime.Event{Kind: realtime.EventAudio, ItemID: "item_a", Audio: []byte{1}})
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "ev_1", ItemID: "item_a", Text: "Hello, "})
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "ev_2", ItemID: "item_a", Text: "this is the clinic."})
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, EventID: "ev_3", ItemID: "item_a", Text: "Hello, this is the clinic."})
	h.setPlayout(src, played(900*ms))
	h.advance(2 * time.Second)
	h.rec.Observe(realtime.Event{Kind: realtime.EventSpeechStarted, EventID: "ev_4", ItemID: "item_b", HasTiming: true, StartMS: 2100})
	h.close(t)

	turns := h.store.latest()
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want agent and caller", len(turns))
	}
	a, c := turns[0], turns[1]
	if a.Speaker != storage.SpeakerAssistant || a.Text != "Hello, this is the clinic." || a.Fidelity != "played" || a.Finality != storage.FinalityFinal {
		t.Fatalf("agent turn = %+v", a)
	}
	if a.ProviderItemID != "item_a" || a.CorrelationID != "item_a" || !slices.Equal(a.SourceIDs, []string{"ev_1", "ev_2", "ev_3"}) {
		t.Fatalf("agent provenance = item %q corr %q sources %v", a.ProviderItemID, a.CorrelationID, a.SourceIDs)
	}
	if a.GeneratedMS == nil || *a.GeneratedMS != 900 || a.PlayedMS == nil || *a.PlayedMS != 900 {
		t.Fatalf("agent playout = %v/%v", a.GeneratedMS, a.PlayedMS)
	}
	if c.Speaker != storage.SpeakerCallee || c.Text != "" || c.Fidelity != "unavailable" || c.ProviderItemID != "item_b" || !hasFlag(c.Quality, FlagCallerUnavailable) {
		t.Fatalf("caller turn = %+v", c)
	}
	flags, complete := h.store.summary(t)
	if complete || !hasFlag(flags, FlagCallerUnavailable) || !hasFlag(flags, FlagTwoPartyIncomplete) {
		t.Fatalf("summary flags %v complete %v, want an incomplete two-party transcript", flags, complete)
	}
}

// Realtime: caller transcription text that arrives anyway is not stored,
// because the V1 contract never enables it.
func TestRealtimeUnexpectedCallerTextIsNotStored(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	h.rec.Observe(realtime.Event{Kind: realtime.EventCallerTranscriptFinal, EventID: "ev_x", ItemID: "item_c", Text: "my secret"})
	h.close(t)
	for _, r := range h.store.latest() {
		if r.Text != "" {
			t.Fatalf("caller text was stored: %+v", r)
		}
	}
	if flags, _ := h.store.summary(t); !hasFlag(flags, FlagUnexpectedCaller) {
		t.Fatalf("flags %v, want %s", flags, FlagUnexpectedCaller)
	}
}

// Realtime: an agent turn starts partial, and a provider final that differs
// from the joined deltas becomes a new, corrected revision. Earlier revisions
// stay as written.
func TestRealtimePartialThenCorrectedFinal(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "ev_1", ItemID: "item_a", Text: "Your table is at sevn"})
	if err := h.rec.flush(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, EventID: "ev_2", ItemID: "item_a", Text: "Your table is at seven."})
	h.setPlayout(audio.SourceRef{ItemID: "item_a"}, played(500*ms))
	h.close(t)

	revs := h.store.revisionsOf(1)
	if len(revs) != 2 {
		t.Fatalf("got %d revisions, want partial then final", len(revs))
	}
	if revs[0].Finality != storage.FinalityPartial || revs[0].Text != "Your table is at sevn" {
		t.Fatalf("first revision = %+v", revs[0])
	}
	if revs[1].Finality != storage.FinalityFinal || revs[1].Text != "Your table is at seven." || !hasFlag(revs[1].Quality, FlagCorrected) {
		t.Fatalf("final revision = %+v", revs[1])
	}
}

// Realtime: final text that only completes the deltas is not a correction.
func TestRealtimeCompletingFinalIsNotCorrected(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "ev_1", ItemID: "item_a", Text: "Hello, "})
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, EventID: "ev_2", ItemID: "item_a", Text: "Hello, world."})
	h.setPlayout(audio.SourceRef{ItemID: "item_a"}, played(500*ms))
	h.close(t)
	if a := h.store.latest()[0]; a.Text != "Hello, world." || hasFlag(a.Quality, FlagCorrected) {
		t.Fatalf("turn = %+v", a)
	}
}

// Realtime: a turn whose transcript never finished is kept as received and
// flagged, never completed by guesswork.
func TestRealtimeMissingDoneIsFlagged(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "ev_1", ItemID: "item_a", Text: "Let me check"})
	h.setPlayout(audio.SourceRef{ItemID: "item_a"}, played(300*ms))
	h.close(t)
	a := h.store.latest()[0]
	if a.Text != "Let me check" || !hasFlag(a.Quality, FlagProviderIncomplete) {
		t.Fatalf("turn = %+v", a)
	}
}

// Agent audio with no transcript at all has no text to show; it is recorded
// as unavailable and flagged, never filled in.
func TestAgentAudioWithoutTranscript(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	h.rec.Observe(realtime.Event{Kind: realtime.EventAudio, ItemID: "item_a", Audio: []byte{1}})
	h.setPlayout(audio.SourceRef{ItemID: "item_a"}, played(400*ms))
	h.close(t)
	a := h.store.latest()[0]
	if a.Text != "" || a.Fidelity != "unavailable" || !hasFlag(a.Quality, FlagAgentMissing) {
		t.Fatalf("turn = %+v", a)
	}
	if flags, _ := h.store.summary(t); !hasFlag(flags, FlagAgentMissing) || !hasFlag(flags, FlagTwoPartyIncomplete) {
		t.Fatalf("summary flags %v", flags)
	}
}

// Interruption: the cut agent turn keeps its full generated text, is labeled as
// including unplayed text, and the cut event names its cause and turn.
func TestInterruptedAgentTurn(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	src := audio.SourceRef{ItemID: "item_a"}
	const text = "Your table for two is booked for seven, and parking is free."
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, EventID: "ev_1", ItemID: "item_a", Text: text})
	h.rec.Observe(realtime.Event{Kind: realtime.EventSpeechStarted, EventID: "ev_2", ItemID: "item_b", HasTiming: true, StartMS: 900})
	h.setPlayout(src, audio.ItemPlayout{Sent: 3000 * ms, Confirmed: 800 * ms, Estimated: 850 * ms, Discarded: 2150 * ms, Interrupted: true})
	h.rec.OnCut(telnyx.Cut{ItemID: "item_a", Source: src, Played: 850 * ms, Sent: 3000 * ms, Audible: true}, 0)
	h.close(t)

	a := h.store.latest()[0]
	if a.Text != text {
		t.Fatal("interrupted text must never be trimmed")
	}
	if a.Fidelity != "interrupted_text_includes_unplayed" || !hasFlag(a.Quality, FlagAgentUnplayed) {
		t.Fatalf("turn = %+v", a)
	}
	if *a.GeneratedMS != 3000 || *a.PlayedMS != 850 || !a.PlayedIsEstimate {
		t.Fatalf("played %v of %v estimate %v", *a.PlayedMS, *a.GeneratedMS, a.PlayedIsEstimate)
	}
	var cut *storage.CallEvent
	for i, e := range h.store.events {
		if e.Kind == "media.barge_in_cut" {
			cut = &h.store.events[i]
		}
	}
	if cut == nil {
		t.Fatalf("no cut event in %v", h.store.eventKinds())
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(cut.PayloadJSON), &p); err != nil {
		t.Fatal(err)
	}
	if cut.CorrelationID != "item_a" || p["caused_by"] != "ev_2" || p["turn_seq"] != float64(1) {
		t.Fatalf("cut event corr %q payload %v", cut.CorrelationID, p)
	}
}

// Audio that was cut before any of it was audible is labeled not played.
func TestAgentTurnCutBeforeAudible(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, EventID: "ev_1", ItemID: "item_a", Text: "Hello?"})
	h.setPlayout(audio.SourceRef{ItemID: "item_a"}, audio.ItemPlayout{Sent: 400 * ms, Discarded: 400 * ms, Interrupted: true})
	h.close(t)
	if a := h.store.latest()[0]; a.Fidelity != "not_played" || !hasFlag(a.Quality, FlagAgentNotPlayed) {
		t.Fatalf("turn = %+v", a)
	}
}

// Text generated with no audio sent was never heard.
func TestAgentTextWithoutAudioIsNotPlayed(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, EventID: "ev_1", ItemID: "item_a", Text: "Goodbye."})
	h.close(t)
	if a := h.store.latest()[0]; a.Fidelity != "not_played" {
		t.Fatalf("turn = %+v", a)
	}
}

// Unconfirmed playout stays partial until the ledger settles, and is labeled
// unconfirmed if it never does.
func TestUnconfirmedPlayout(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	src := audio.SourceRef{ItemID: "item_a"}
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, EventID: "ev_1", ItemID: "item_a", Text: "One moment."})
	h.setPlayout(src, audio.ItemPlayout{Sent: 800 * ms, Confirmed: 600 * ms, Estimated: 600 * ms})
	if err := h.rec.flush(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if r := h.store.revisionsOf(1); len(r) != 1 || r[0].Finality != storage.FinalityPartial {
		t.Fatalf("unsettled playout must stay partial: %+v", r)
	}
	h.close(t)
	a := h.store.latest()[0]
	if a.Finality != storage.FinalityFinal || a.Fidelity != "playout_unconfirmed" || !hasFlag(a.Quality, FlagPlayoutUnconfirmed) {
		t.Fatalf("turn = %+v", a)
	}
}

// A replayed provider event is recorded once.
func TestDuplicateEventsRecordedOnce(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	ev := realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "ev_1", ItemID: "item_a", Text: "Hi."}
	h.rec.Observe(ev)
	h.rec.Observe(ev)
	h.close(t)
	if a := h.store.latest()[0]; a.Text != "Hi." || len(a.SourceIDs) != 1 {
		t.Fatalf("turn = %+v", a)
	}
}

// Live: both parties come from native transcript deltas with provider timing.
// Fragments are grouped into turns on the provider timeline and joined
// verbatim; agent turns are reconciled per local utterance.
func TestLiveBothPartiesWithProvenance(t *testing.T) {
	h := newHarness(t, realtime.LiveCapabilities)
	in := func(id, text string, s, e int64) {
		h.rec.Observe(realtime.Event{Kind: realtime.EventCallerTranscriptDelta, EventID: id, Text: text, HasTiming: true, StartMS: s, EndMS: e})
	}
	out := func(id, text string, s, e int64) {
		h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: id, Text: text, HasTiming: true, StartMS: s, EndMS: e})
	}
	out("o1", "Hi, ", 0, 300)
	out("o2", "how can I help?", 300, 900)
	h.setPlayout(audio.SourceRef{Utterance: 1}, played(900*ms))
	h.nextUtterance()
	in("i1", "I'd like ", 1200, 1500)
	in("i2", "a table.", 1500, 1900)
	out("o3", "Sure.", 2200, 2500)
	h.setPlayout(audio.SourceRef{Utterance: 2}, played(300*ms))
	h.close(t)

	turns := h.store.latest()
	if len(turns) != 3 {
		t.Fatalf("got %d turns: %+v", len(turns), turns)
	}
	want := []struct {
		speaker, text, fidelity string
		start, end              int64
	}{
		{storage.SpeakerAssistant, "Hi, how can I help?", "played", 0, 900},
		{storage.SpeakerCallee, "I'd like a table.", "provider_asr_approximate", 1200, 1900},
		{storage.SpeakerAssistant, "Sure.", "played", 2200, 2500},
	}
	for i, w := range want {
		g := turns[i]
		if g.Speaker != w.speaker || g.Text != w.text || g.Fidelity != w.fidelity || g.StartedMS != w.start || g.EndedMS == nil || *g.EndedMS != w.end {
			t.Fatalf("turn %d = %+v, want %+v", i+1, g, w)
		}
		if g.TimingSource != storage.TimingProvider || g.ProviderItemID != "" || !hasFlag(g.Quality, FlagGroupedLocally) {
			t.Fatalf("turn %d provenance = %+v", i+1, g)
		}
	}
	if turns[0].CorrelationID != "utterance:1" || turns[2].CorrelationID != "utterance:2" {
		t.Fatalf("agent correlation %q %q", turns[0].CorrelationID, turns[2].CorrelationID)
	}
	if !slices.Equal(turns[1].SourceIDs, []string{"i1", "i2"}) {
		t.Fatalf("caller sources %v", turns[1].SourceIDs)
	}
	flags, complete := h.store.summary(t)
	if !complete || hasFlag(flags, FlagCallerUnavailable) || hasFlag(flags, FlagTwoPartyIncomplete) {
		t.Fatalf("summary flags %v complete %v, want a complete two-party transcript", flags, complete)
	}
}

// Live: a fragment that arrives out of order is placed by its provider start
// time and flagged; a gap past CallerTurnGap starts a new caller turn.
func TestLiveOutOfOrderAndGap(t *testing.T) {
	h := newHarness(t, realtime.LiveCapabilities)
	in := func(id, text string, s, e int64) {
		h.rec.Observe(realtime.Event{Kind: realtime.EventCallerTranscriptDelta, EventID: id, Text: text, HasTiming: true, StartMS: s, EndMS: e})
	}
	in("i2", "world", 500, 900)
	in("i1", "hello ", 100, 500)
	in("i3", "later", 900+CallerTurnGap.Milliseconds()+1, 4000)
	h.close(t)
	turns := h.store.latest()
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want two caller turns split by the gap", len(turns))
	}
	if turns[0].Text != "hello world" || turns[0].StartedMS != 100 || !hasFlag(turns[0].Quality, FlagOutOfOrder) {
		t.Fatalf("first turn = %+v", turns[0])
	}
	if turns[1].Text != "later" {
		t.Fatalf("second turn = %+v", turns[1])
	}
}

// Live: a caller fragment after an agent fragment on the provider timeline
// starts a new caller turn.
func TestLiveAgentSplitsCallerTurns(t *testing.T) {
	h := newHarness(t, realtime.LiveCapabilities)
	h.rec.Observe(realtime.Event{Kind: realtime.EventCallerTranscriptDelta, EventID: "i1", Text: "Yes.", HasTiming: true, StartMS: 0, EndMS: 300})
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "o1", Text: "Great.", HasTiming: true, StartMS: 400, EndMS: 600})
	h.rec.Observe(realtime.Event{Kind: realtime.EventCallerTranscriptDelta, EventID: "i2", Text: "Thanks.", HasTiming: true, StartMS: 700, EndMS: 900})
	h.close(t)
	turns := h.store.latest()
	if len(turns) != 3 || turns[0].Text != "Yes." || turns[2].Text != "Thanks." || turns[2].Speaker != storage.SpeakerCallee {
		t.Fatalf("turns = %+v", turns)
	}
}

// A failed write loses nothing: the next flush rewrites the turns and events.
func TestWriteFailureRetries(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "ev_1", ItemID: "item_a", Text: "Hi"})
	h.store.fail = errors.New("disk full")
	if err := h.rec.flush(context.Background(), false); err == nil {
		t.Fatal("flush hid a store failure")
	}
	h.store.fail = nil
	h.close(t)
	if a := h.store.latest(); len(a) != 1 || a[0].Text != "Hi" {
		t.Fatalf("turns after retry = %+v", a)
	}
	kinds := h.store.eventKinds()
	if kinds[0] != storage.TranscriptStartedKind || kinds[len(kinds)-1] != storage.TranscriptFinalizedKind {
		t.Fatalf("events after retry = %v", kinds)
	}
}

// Past the turn bound new turns are dropped and flagged, never silently cut.
func TestTurnBound(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	for i := range MaxTurns + 1 {
		h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, ItemID: "item_" + strconv.Itoa(i), Text: "x"})
	}
	h.close(t)
	if n := len(h.store.latest()); n != MaxTurns {
		t.Fatalf("stored %d turns, want %d", n, MaxTurns)
	}
	if flags, _ := h.store.summary(t); !hasFlag(flags, FlagEventsDropped) {
		t.Fatalf("flags %v", flags)
	}
}

// A long call with many sequential turns, beyond the old per-call event and
// replay-id bounds, persists every turn and its done event. Pending events are
// written by periodic flushes, so the event bound applies per flush window.
func TestLongCallSequentialTurnsAllPersisted(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	const n = 4500
	for i := range n {
		id := "item_" + strconv.Itoa(i)
		h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "d_" + strconv.Itoa(i), ItemID: id, Text: "a"})
		h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, EventID: "f_" + strconv.Itoa(i), ItemID: id, Text: "a"})
		h.setPlayout(audio.SourceRef{ItemID: id}, played(100*ms))
		h.rec.Observe(realtime.Event{Kind: realtime.EventResponseDone, RawType: "response.done", EventID: "r_" + strconv.Itoa(i), ResponseID: "resp_" + strconv.Itoa(i), ResponseStatus: "completed"})
		if i%100 == 99 {
			if err := h.rec.flush(context.Background(), false); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.close(t)
	turns := h.store.latest()
	if len(turns) != n {
		t.Fatalf("persisted %d turns, want %d", len(turns), n)
	}
	if flags, _ := h.store.summary(t); hasFlag(flags, FlagEventsDropped) {
		t.Fatalf("flags %v: a long call dropped data", flags)
	}
	doneEvents := 0
	for _, e := range h.store.events {
		if e.Kind == "provider.response_done" {
			doneEvents++
		}
	}
	if doneEvents != n {
		t.Fatalf("persisted %d response_done events, want %d", doneEvents, n)
	}
}

// Provider events keep ids and correlation but never transcript text or audio.
func TestProviderEventsCarryNoContent(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	h.rec.Observe(realtime.Event{Kind: realtime.EventResponseDone, RawType: "response.done", EventID: "ev_9", ResponseID: "resp_1", ResponseStatus: "completed", Text: "secret"})
	h.close(t)
	for _, e := range h.store.events {
		if e.Kind == "provider.response_done" {
			if e.ProviderEventID != "ev_9" || e.CorrelationID != "resp_1" {
				t.Fatalf("event = %+v", e)
			}
			var p map[string]any
			_ = json.Unmarshal([]byte(e.PayloadJSON), &p)
			for _, v := range p {
				if v == "secret" {
					t.Fatalf("payload leaks text: %s", e.PayloadJSON)
				}
			}
			return
		}
	}
	t.Fatalf("no response_done event in %v", h.store.eventKinds())
}

// A second media run for the same call continues turn numbering after the
// first run's turns, so the earlier turns are not revised.
func TestSecondRunContinuesSeq(t *testing.T) {
	store := &memStore{}
	run := func(text string) {
		rec, err := New(Config{
			CallID: "call_1", OwnerUserID: "alice", Caps: realtime.RealtimeCapabilities, Store: store,
			Playout:  func(audio.SourceRef) (audio.ItemPlayout, bool) { return played(300 * ms), true },
			SourceOf: func(ev realtime.Event) audio.SourceRef { return audio.SourceRef{ItemID: ev.ItemID} },
		})
		if err != nil {
			t.Fatal(err)
		}
		rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, EventID: "ev_" + text, ItemID: "item_" + text, Text: text})
		if err := rec.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	run("first")
	run("second")
	turns := store.latest()
	if len(turns) != 2 || turns[0].Seq != 1 || turns[1].Seq != 2 || turns[0].Text != "first" || turns[1].Text != "second" {
		t.Fatalf("turns after second run = %+v", turns)
	}
	if n := len(store.revisionsOf(1)); n != 1 {
		t.Fatalf("first run's turn was revised: %d revisions", n)
	}
}

// Live: a late caller fragment from before an agent reply joins its own caller
// turn. It is not attached to the latest caller turn, and the agent turn stays
// between them.
func TestLateCallerFragmentJoinsItsOwnTurn(t *testing.T) {
	h := newHarness(t, realtime.LiveCapabilities)
	in := func(id, text string, s, e int64) {
		h.rec.Observe(realtime.Event{Kind: realtime.EventCallerTranscriptDelta, EventID: id, Text: text, HasTiming: true, StartMS: s, EndMS: e})
	}
	in("i1", "Book ", 0, 300)
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "o1", Text: "Sure.", HasTiming: true, StartMS: 400, EndMS: 600})
	in("i3", "Friday", 700, 900)
	in("i2", "a table", 300, 700)
	h.close(t)
	turns := h.store.latest()
	if len(turns) != 3 {
		t.Fatalf("got %d turns: %+v", len(turns), turns)
	}
	if turns[0].Text != "Book a table" || turns[0].StartedMS != 0 {
		t.Fatalf("caller turn 1 = %+v", turns[0])
	}
	if turns[1].Speaker != storage.SpeakerAssistant || turns[1].Text != "Sure." {
		t.Fatalf("agent turn = %+v", turns[1])
	}
	if turns[2].Speaker != storage.SpeakerCallee || turns[2].Text != "Friday" {
		t.Fatalf("later caller turn = %+v", turns[2])
	}
}

// An oversized provider item id is dropped from the correlation and flagged,
// and the turn still persists. It must not fail the whole transaction.
func TestOversizedProviderIDIsDroppedAndFlagged(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	long := strings.Repeat("x", maxProviderID+1)
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, EventID: "ev_1", ItemID: long, Text: "Hello."})
	h.setPlayout(audio.SourceRef{ItemID: long}, played(300*ms))
	h.close(t)
	a := h.store.latest()
	if len(a) != 1 || a[0].Text != "Hello." || a[0].CorrelationID != "" || !hasFlag(a[0].Quality, FlagProviderIDInvalid) {
		t.Fatalf("turn = %+v", a)
	}
}

// Live: a caller speech edge falls between an agent's audio and its late
// transcript delta. The agent text was sent and audible, so it must not be
// claimed not_played, and the turn carries the local attribution flag.
func TestLiveAgentTurnSplitByCallerEdgeIsNotNotPlayed(t *testing.T) {
	h := newHarness(t, realtime.LiveCapabilities)
	h.rec.Observe(realtime.Event{Kind: realtime.EventAudio, Audio: []byte{1}})
	h.setPlayout(audio.SourceRef{Utterance: 1}, played(500*ms))
	h.nextUtterance()
	h.rec.Observe(realtime.Event{Kind: realtime.EventSpeechStarted, EventID: "sp_1", HasTiming: true, StartMS: 200})
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "o1", Text: "Sure thing.", HasTiming: true, StartMS: 300, EndMS: 600})
	h.close(t)
	for _, a := range h.store.latest() {
		if a.Speaker != storage.SpeakerAssistant || a.Text != "Sure thing." {
			continue
		}
		if a.Fidelity == "not_played" || hasFlag(a.Quality, FlagAgentNotPlayed) {
			t.Fatalf("late agent text claimed not played: %+v", a)
		}
		if !hasFlag(a.Quality, FlagPlayoutAttribution) {
			t.Fatalf("live agent turn lacks %s: %+v", FlagPlayoutAttribution, a)
		}
		return
	}
	t.Fatal("no agent turn with the late text recorded")
}

// Live: text with no sent audio is playout_unconfirmed rather than a claim
// that it was never played.
func TestLiveTextWithoutAudioIsUnconfirmed(t *testing.T) {
	h := newHarness(t, realtime.LiveCapabilities)
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "o1", Text: "Hello.", HasTiming: true, StartMS: 0, EndMS: 300})
	h.close(t)
	for _, a := range h.store.latest() {
		if a.Speaker == storage.SpeakerAssistant && (a.Fidelity != "playout_unconfirmed" || hasFlag(a.Quality, FlagAgentNotPlayed)) {
			t.Fatalf("live text without audio = %+v", a)
		}
	}
}

// A playout-only change after the final revision sets playout_updated, not
// corrected. A text change still sets corrected.
func TestPlayoutUpdateIsNotCorrection(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	src := audio.SourceRef{ItemID: "item_a"}
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, EventID: "ev_1", ItemID: "item_a", Text: "Done."})
	// Unconfirmed audio that settles: the turn becomes final, labeled unconfirmed.
	h.setPlayout(src, audio.ItemPlayout{Sent: 800 * ms, Confirmed: 600 * ms, Estimated: 600 * ms, Unconfirmed: 200 * ms})
	if err := h.rec.flush(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	// Later playout evidence changes, the text does not.
	h.setPlayout(src, played(800*ms))
	h.close(t)
	revs := h.store.revisionsOf(1)
	if len(revs) < 2 {
		t.Fatalf("want a final revision then a playout update, got %+v", revs)
	}
	for _, r := range revs {
		if hasFlag(r.Quality, FlagCorrected) {
			t.Fatalf("playout-only change marked corrected: %+v", r)
		}
	}
	last := revs[len(revs)-1]
	if !hasFlag(last.Quality, FlagPlayoutUpdated) || last.Text != "Done." || last.Fidelity != "played" {
		t.Fatalf("last revision = %+v, want played with %s", last, FlagPlayoutUpdated)
	}
}

// A final turn whose playout changes after it was written gets a new revision on
// the next periodic flush, before Close. Playout progress alone must not wait
// for the call to end.
func TestFinalTurnPlayoutChangePersistedOnPeriodicFlush(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	src := audio.SourceRef{ItemID: "item_a"}
	h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDone, EventID: "ev_1", ItemID: "item_a", Text: "Done."})
	h.setPlayout(src, played(800*ms))
	if err := h.rec.flush(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if n := len(h.store.revisionsOf(1)); n != 1 {
		t.Fatalf("after first flush: %d revisions, want 1", n)
	}
	// Playout now reports that audio was interrupted: the turn stays final, but
	// its playout evidence changed. No text changed and the call is still open.
	h.setPlayout(src, audio.ItemPlayout{Sent: 800 * ms, Confirmed: 300 * ms, Estimated: 300 * ms, Discarded: 500 * ms, Interrupted: true})
	if err := h.rec.flush(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	revs := h.store.revisionsOf(1)
	if len(revs) != 2 {
		t.Fatalf("after periodic flush with a playout change: %d revisions, want 2 (not held until Close)", len(revs))
	}
	if got := revs[1]; got.Text != "Done." || got.Fidelity != "interrupted_text_includes_unplayed" || hasFlag(got.Quality, FlagCorrected) {
		t.Fatalf("playout revision = %+v", got)
	}
}

// Past the per-call text budget, further text is dropped and flagged, while the
// text already held stays. A long call has a bounded memory ceiling.
func TestCallTextBudgetIsBounded(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	chunk := strings.Repeat("a", MaxTurnText)
	// Each turn can hold MaxTurnText, so enough turns exceed the call budget.
	turns := MaxCallText/MaxTurnText + 2
	for i := range turns {
		id := "item_" + strconv.Itoa(i)
		h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, EventID: "d_" + strconv.Itoa(i), ItemID: id, Text: chunk})
	}
	h.close(t)
	if r := h.rec.callText; r > MaxCallText {
		t.Fatalf("call text %d exceeds budget %d", r, MaxCallText)
	}
	if flags, _ := h.store.summary(t); !hasFlag(flags, FlagEventsDropped) {
		t.Fatalf("flags %v, want %s once the text budget is reached", flags, FlagEventsDropped)
	}
}

// A cut with no sent audio omits played_ms and sent_ms instead of writing 0.
func TestCutWithoutAudioOmitsUnknownDurations(t *testing.T) {
	h := newHarness(t, realtime.RealtimeCapabilities)
	h.rec.OnCut(telnyx.Cut{}, 0)
	h.close(t)
	for _, e := range h.store.events {
		if e.Kind != "media.barge_in_cut" {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(e.PayloadJSON), &p); err != nil {
			t.Fatal(err)
		}
		if _, ok := p["played_ms"]; ok {
			t.Fatalf("unknown played_ms written as a value: %v", p)
		}
		if _, ok := p["sent_ms"]; ok {
			t.Fatalf("unknown sent_ms written as a value: %v", p)
		}
		return
	}
	t.Fatal("no cut event")
}

// Observe, OnCut and periodic flushes may run concurrently.
func TestConcurrentObserve(t *testing.T) {
	h := newHarness(t, realtime.LiveCapabilities)
	h.rec.Start()
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				h.rec.Observe(realtime.Event{Kind: realtime.EventAgentTranscriptDelta, Text: "x", HasTiming: true, StartMS: int64(g*1000 + i), EndMS: int64(g*1000 + i + 1)})
				if i%50 == 0 {
					h.rec.OnCut(telnyx.Cut{Source: audio.SourceRef{Utterance: 1}}, uint64(i))
					_ = h.rec.flush(context.Background(), false)
				}
			}
		}()
	}
	wg.Wait()
	h.close(t)
	if a := h.store.latest(); len(a) != 1 || len(a[0].Text) != 800 {
		t.Fatalf("got %d turns", len(a))
	}
}
