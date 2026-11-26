package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
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
