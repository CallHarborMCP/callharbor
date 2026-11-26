// Package transcript turns the normalized voice events of one call into stored
// transcript turns and a correlated event timeline. It reuses the fidelity
// rules of package recording: agent text is reconciled with playout evidence
// and is never trimmed, and caller text that the protocol does not supply is
// recorded as unavailable, never guessed.
package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/recording"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// Quality flags. They appear on turns and, unioned, on the transcript summary.
const (
	FlagCallerUnavailable  = "caller_transcript_unavailable"
	FlagTwoPartyIncomplete = "two_party_incomplete"
	FlagAgentUnplayed      = "agent_text_includes_unplayed"
	FlagPlayoutUnconfirmed = "playout_unconfirmed"
	FlagAgentNotPlayed     = "agent_text_not_played"
	FlagAgentMissing       = "agent_transcript_missing"
	FlagEventsDropped      = "events_dropped"
	FlagGroupedLocally     = "turns_grouped_locally"
	FlagTimingLocal        = "timing_local_receive"
	FlagProviderIncomplete = "provider_turn_incomplete"
	FlagOutOfOrder         = "out_of_order_fragments"
	FlagCorrected          = "corrected"
	FlagUnexpectedCaller   = "unexpected_caller_transcription_ignored"
	FlagTruncatedText      = "text_truncated"
	// FlagPlayoutAttribution marks a Live agent turn: its utterance grouping
	// and playout accounting are a local guess, not provider evidence.
	FlagPlayoutAttribution = "playout_attribution_local"
	// FlagPlayoutUpdated marks playout evidence that changed after the final
	// text was written. The text itself did not change.
	FlagPlayoutUpdated = "playout_updated"
	// FlagProviderIDInvalid marks a provider identifier that was too long to
	// store and was dropped.
	FlagProviderIDInvalid = "provider_id_invalid"
	// FlagCallerPending marks a caller turn whose out-of-band transcription has
	// not resolved yet. A pending turn is never presented as complete.
	FlagCallerPending = "caller_transcript_pending"
	// FlagCallerTextModelDerived marks caller text produced by an
	// out-of-band response of the configured voice model. It is approximate.
	FlagCallerTextModelDerived = "caller_text_model_derived"
	// FlagCallerFailed marks a caller turn whose out-of-band transcription
	// failed, was rejected, timed out, or was cancelled.
	FlagCallerFailed = "caller_transcript_failed"
	// FlagCallerUnresolved marks a caller turn still pending when the call
	// ended.
	FlagCallerUnresolved = "caller_transcript_unresolved"
	// FlagCallerEmpty marks a caller turn whose model-derived text is empty.
	FlagCallerEmpty = "caller_text_empty"
)

// maxProviderID is the longest provider identifier stored. It matches the
// correlation_id CHECK in the transcript and call event schema.
const maxProviderID = 128

// Bounds on what one call may hold in memory and in storage. Past a bound the
// excess is dropped and flagged, never silently cut.
const (
	// MaxTurns is a per-call memory ceiling, far above a 7200 s call's turns.
	MaxTurns = 20000
	// MaxTurnText bounds the text of one turn.
	MaxTurnText = 32 << 10
	// MaxCallText bounds the transcript text of one call across all turns, so
	// the worst case is MaxCallText of text plus per-turn overhead, not
	// MaxTurns × MaxTurnText. Text beyond it is dropped and flagged.
	MaxCallText = 4 << 20
	// MaxFragments bounds the fragments of one turn. MaxCallFragments bounds
	// them across the call.
	MaxFragments     = 4096
	MaxCallFragments = 1 << 17
	// MaxTurnSources bounds the provider ids one turn keeps. MaxCallSources
	// bounds them across the call.
	MaxTurnSources = 256
	MaxCallSources = 1 << 17
	// MaxEvents bounds events waiting to be written. Written events leave memory.
	MaxEvents = 4000
	// maxSeenEvents bounds the provider event ids kept for replay detection.
	maxSeenEvents  = 8192
	FlushInterval  = time.Second
	CallerTurnGap  = 2 * time.Second
	finalizeBudget = 10 * time.Second
)

// Store persists transcript revisions and call events.
type Store interface {
	RecordTranscript(ctx context.Context, callID, ownerUserID string, turns []storage.TranscriptRevision, events []storage.CallEvent) error
	MaxTranscriptSeq(ctx context.Context, ownerUserID, callID string) (int, error)
}

// Config wires a Recorder to one call.
type Config struct {
	CallID      string
	OwnerUserID string
	Caps        realtime.Capabilities
	Store       Store
	// Playout returns the playout accounting of an agent audio source.
	Playout func(audio.SourceRef) (audio.ItemPlayout, bool)
	// SourceOf maps an agent event to its playout source. It must return the
	// same source the bridge tagged the event's audio with.
	SourceOf func(realtime.Event) audio.SourceRef
	Now      func() time.Time
	Logger   *slog.Logger
}

type fragment struct {
	text    string
	startMS int64
	endMS   int64
	timed   bool
}

type turn struct {
	seq     int
	speaker recording.Speaker
	src     audio.SourceRef
	itemID  string
	corr    string
	frags   []fragment
	// done holds the provider's final text (Realtime transcript done).
	done     *string
	sources  []string
	timed    bool
	startMS  int64
	endMS    int64
	hasEnd   bool
	flags    map[string]bool
	dirty    bool
	wroteFin bool
	// finalText is the text of the last final revision written.
	finalText string
	// playoutSeen is the playout accounting the last written revision used.
	playoutSeen audio.ItemPlayout
	unavail     bool
	// pending is set while an out-of-band caller transcription is unresolved.
	pending bool
	// failed is set when the out-of-band transcription failed.
	failed bool
	// modelDerived is set for caller text produced by an out-of-band response.
	modelDerived bool
	lastWrite    string
	textBytes    int
	// doneBytes is the length of the provider's final text, counted against
	// MaxTurnText and MaxCallText.
	doneBytes int
}

// truncateUTF8 returns the longest prefix of s of at most n bytes that ends on
// a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Recorder collects the events of one call. Observe and OnCut may be called
// from any goroutine. It is not reusable after Close.
//
// Memory bound per call (worst case, all caps reached, approximate):
//   - transcript text (deltas and final text): MaxCallText (4 MiB) across all
//     turns, then truncated and flagged text_truncated.
//   - fragment structs: MaxCallFragments (2^17 = 131072) × ~48 B ≈ 6 MiB, then
//     flagged events_dropped. MaxFragments (4096) bounds each turn.
//   - turns: MaxTurns (20000) × ~300 B of turn overhead ≈ 6 MiB, excluding
//     fragments and text.
//   - source IDs: MaxCallSources (2^17) × ~64 B ≈ 8 MiB, then flagged
//     events_dropped. MaxTurnSources (256) bounds each turn.
//   - provider IDs: each stored id is at most maxProviderID (128) bytes; longer
//     ids are dropped and flagged provider_id_invalid.
//   - replay IDs: maxSeenEvents (8192) rolling window, ~0.5 MiB.
//   - pending events: MaxEvents (4000) until the next flush writes them.
//
// Total, with every cap reached: about 25 MiB plus text. The per-call text
// budget is the dominant term and is itself bounded at 4 MiB.
//
// A 7200 s call produces far fewer turns and text than these ceilings, so the
// bound is not reached in practice; it is what keeps a runaway provider bounded.
type Recorder struct {
	cfg   Config
	start time.Time
	live  bool
	// seqBase is the highest seq already stored for the call before this run.
	seqBase int
	// nSeq counts the turns this run has created.
	nSeq int
	// callText and callFrags are the text bytes and fragments this run holds
	// across all turns.
	callText    int
	callFrags   int
	callSources int

	mu         sync.Mutex
	turns      []*turn
	agentBySrc map[audio.SourceRef]*turn
	callerByID map[string]*turn
	seenEvents map[string]bool
	seenOrder  []string
	events     []storage.CallEvent
	flags      map[string]bool
	lastCut    string

	// flushMu serializes writes, so revisions are stored in the order built.
	flushMu sync.Mutex
	started bool
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// New returns a recorder. Start begins periodic persistence.
func New(cfg Config) (*Recorder, error) {
	if cfg.CallID == "" || cfg.OwnerUserID == "" || cfg.Store == nil || cfg.Playout == nil || cfg.SourceOf == nil {
		return nil, fmt.Errorf("transcript: incomplete recorder config")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	// A later media run for the same call continues numbering after the
	// turns already stored, so it never revises them.
	base, err := cfg.Store.MaxTranscriptSeq(context.Background(), cfg.OwnerUserID, cfg.CallID)
	if err != nil {
		return nil, fmt.Errorf("transcript: load turn base: %w", err)
	}
	r := &Recorder{
		cfg:        cfg,
		start:      cfg.Now(),
		live:       cfg.Caps.Protocol == realtime.ProtocolLive,
		seqBase:    base,
		agentBySrc: map[audio.SourceRef]*turn{},
		callerByID: map[string]*turn{},
		seenEvents: map[string]bool{},
		flags:      map[string]bool{},
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	if r.live {
		r.flags[FlagGroupedLocally] = true
	}
	r.addEventLocked(storage.CallEvent{
		Kind: storage.TranscriptStartedKind, Provider: "internal",
		PayloadJSON: r.summaryJSON(map[string]any{"protocol": string(cfg.Caps.Protocol)}),
	})
	return r, nil
}

// Start persists collected turns every FlushInterval until Close.
func (r *Recorder) Start() {
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	go func() {
		defer close(r.done)
		t := time.NewTicker(FlushInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				// A failed write is logged and retried on the next tick.
				_ = r.flush(context.Background(), false)
			}
		}
	}()
}

// Close stops periodic persistence and writes the final revisions and the
// transcript.finalized summary. It is safe to call more than once.
func (r *Recorder) Close(ctx context.Context) error {
	var err error
	r.once.Do(func() {
		close(r.stop)
		r.mu.Lock()
		started := r.started
		r.mu.Unlock()
		if started {
			select {
			case <-r.done:
			case <-ctx.Done():
			}
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizeBudget)
		defer cancel()
		err = r.flush(ctx, true)
	})
	return err
}

func (r *Recorder) sinceStart() int64 {
	ms := r.cfg.Now().Sub(r.start).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

// Observe records one normalized provider event. It never blocks on storage.
func (r *Recorder) Observe(ev realtime.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// An oversized event id is neither stored nor remembered for replay. Item
	// and response ids are bounded where they are stored, not here, because an
	// agent item id is still needed to find its playout accounting.
	if len(ev.EventID) > maxProviderID {
		r.flags[FlagProviderIDInvalid] = true
		ev.EventID = ""
	}
	if ev.EventID != "" && (ev.Kind == realtime.EventAgentTranscriptDelta || ev.Kind == realtime.EventAgentTranscriptDone ||
		ev.Kind == realtime.EventCallerTranscriptDelta || ev.Kind == realtime.EventCallerTranscriptFinal) {
		if r.seenEvents[ev.EventID] {
			// A replayed provider event is recorded once.
			return
		}
		r.rememberEventLocked(ev.EventID)
	}
	now := r.sinceStart()
	switch ev.Kind {
	case realtime.EventAudio:
		t := r.agentTurnLocked(r.cfg.SourceOf(ev), now)
		if t != nil {
			r.touchLocal(t, now)
		}
	case realtime.EventAgentTranscriptDelta:
		t := r.agentTurnLocked(r.cfg.SourceOf(ev), now)
		r.addFragmentLocked(t, ev, now)
	case realtime.EventAgentTranscriptDone:
		t := r.agentTurnLocked(r.cfg.SourceOf(ev), now)
		if t == nil {
			return
		}
		text := ev.Text
		// A replacement final text replaces the previous one, so the previous
		// length is released from the call budget first. The allowance is then
		// computed against what the call still holds, not the old text.
		r.callText -= t.doneBytes
		room := min(MaxTurnText, MaxCallText-r.callText)
		if len(text) > room {
			// Final text counts against the same text budget as deltas. It is
			// cut at a rune boundary and flagged, never silently truncated.
			text = truncateUTF8(text, max(room, 0))
			t.flags[FlagTruncatedText] = true
			r.flags[FlagTruncatedText] = true
		}
		t.doneBytes = len(text)
		r.callText += len(text)
		// Final text that only completes the deltas is not a correction;
		// final text that rewrites them is.
		if joined := joinText(t.frags); len(t.frags) > 0 && !strings.HasPrefix(text, joined) {
			t.flags[FlagCorrected] = true
		}
		t.done = &text
		r.addSourceLocked(t, ev.EventID)
		r.touchLocal(t, now)
		t.dirty = true
	case realtime.EventCallerTranscriptDelta, realtime.EventCallerTranscriptFinal:
		if !r.callerTextAllowedLocked(ev) {
			// Separately billed transcription is never enabled. Caller text
			// that is not out-of-band on an out-of-band call is outside the
			// contract and is not stored.
			r.flags[FlagUnexpectedCaller] = true
			return
		}
		r.callerTextLocked(ev, now)
	case realtime.EventSpeechStarted:
		r.callerSpeechLocked(ev, now)
	case realtime.EventCallerTranscriptFailed:
		r.callerFailedLocked(ev, now)
	case realtime.EventTruncated, realtime.EventResponseDone, realtime.EventDelegation,
		realtime.EventError, realtime.EventClosed:
		r.providerEventLocked(ev)
	}
}

// OnCut records a barge-in cut with its cause. callerTurn is the local caller
// turn that caused it, or zero for a provider speech-start barge-in.
func (r *Recorder) OnCut(c telnyx.Cut, callerTurn uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	payload := map[string]any{"audible": c.Audible}
	// Played and Sent are measured only for a cut that had an item with
	// audio sent. Otherwise they are unknown, and are omitted rather than 0.
	if c.Sent > 0 {
		payload["played_ms"], payload["sent_ms"] = c.Played.Milliseconds(), c.Sent.Milliseconds()
	}
	cause := r.lastCut
	if callerTurn > 0 {
		cause = "caller_turn:" + strconv.FormatUint(callerTurn, 10)
	}
	if cause != "" {
		payload["caused_by"] = cause
	}
	corr := correlationOf(c.Source)
	if t, ok := r.agentBySrc[c.Source]; ok && c.Source != (audio.SourceRef{}) {
		payload["turn_seq"] = t.seq
		t.dirty = true
	}
	r.addEventLocked(storage.CallEvent{
		Kind: "media.barge_in_cut", Provider: "internal", CorrelationID: corr, PayloadJSON: mustJSON(payload),
	})
}

func (r *Recorder) agentTurnLocked(src audio.SourceRef, now int64) *turn {
	if t, ok := r.agentBySrc[src]; ok {
		return t
	}
	t := r.newTurnLocked(recording.SpeakerAgent, now)
	if t == nil {
		return nil
	}
	t.src = src
	t.itemID = boundItemID(t, src.ItemID)
	t.corr = boundCorrelation(t, correlationOf(src))
	r.agentBySrc[src] = t
	return t
}

// callerTurnLocked returns the caller turn a caller transcript fragment joins.
// Realtime fragments are keyed by item_id. Live fragments have no item or turn
// marker, so they are grouped on the provider timeline: a fragment joins the
// latest caller turn unless an agent fragment started between them or the gap
// exceeds CallerTurnGap.
func (r *Recorder) callerTurnLocked(ev realtime.Event, now int64) *turn {
	if ev.ItemID != "" {
		if t, ok := r.callerByID[ev.ItemID]; ok {
			return t
		}
		t := r.newTurnLocked(recording.SpeakerCaller, now)
		if t != nil {
			t.itemID, t.corr = boundItemID(t, ev.ItemID), boundCorrelation(t, ev.ItemID)
			r.callerByID[ev.ItemID] = t
		}
		return t
	}
	if ev.HasTiming {
		if t := r.nearestCallerTurnLocked(ev.StartMS); t != nil {
			return t
		}
	}
	t := r.newTurnLocked(recording.SpeakerCaller, now)
	if t != nil {
		t.corr = "caller:" + strconv.Itoa(t.seq)
	}
	return t
}

// nearestCallerTurnLocked returns the timed caller turn a Live fragment at
// startMS belongs to. It is the caller turn whose window contains the fragment
// or is nearest to it in time, provided the distance is within CallerTurnGap
// and no agent fragment starts between the fragment and that turn. A late
// fragment therefore joins its own turn, and an intervening agent turn is never
// passed over.
func (r *Recorder) nearestCallerTurnLocked(startMS int64) *turn {
	var best *turn
	var bestDist int64
	for _, t := range r.turns {
		if t.speaker != recording.SpeakerCaller || t.unavail || !t.timed {
			continue
		}
		d := distanceTo(startMS, t.startMS, t.endMS)
		if best == nil || d < bestDist {
			best, bestDist = t, d
		}
	}
	if best == nil || bestDist > CallerTurnGap.Milliseconds() {
		return nil
	}
	if r.agentStartedBetweenLocked(min(startMS, best.startMS), max(startMS, best.startMS)) {
		return nil
	}
	return best
}

// distanceTo is the distance from x to the window [lo, hi], zero inside it.
func distanceTo(x, lo, hi int64) int64 {
	switch {
	case x < lo:
		return lo - x
	case x > hi:
		return x - hi
	}
	return 0
}

// agentStartedBetweenLocked reports whether an agent fragment starts on the
// provider timeline strictly between lo and hi.
func (r *Recorder) agentStartedBetweenLocked(lo, hi int64) bool {
	for _, t := range r.turns {
		if t.speaker != recording.SpeakerAgent {
			continue
		}
		for _, f := range t.frags {
			if f.timed && f.startMS > lo && f.startMS < hi {
				return true
			}
		}
	}
	return false
}

// callerSpeechLocked records a native caller speech start. On a protocol whose
// caller text is unavailable, it becomes an explicit caller turn with no text,
// so the transcript shows the caller spoke without inventing what was said.
func (r *Recorder) callerSpeechLocked(ev realtime.Event, now int64) {
	r.lastCut = ev.EventID
	r.providerEventLocked(ev)
	if ev.ItemID == "" {
		return
	}
	switch r.cfg.Caps.V1CallerTranscript() {
	case realtime.OutOfBand:
		// The caller turn exists from speech start and stays pending until its
		// out-of-band transcription resolves.
		if _, ok := r.callerByID[ev.ItemID]; ok {
			return
		}
		t := r.newTurnLocked(recording.SpeakerCaller, now)
		if t == nil {
			return
		}
		t.itemID, t.corr, t.pending = boundItemID(t, ev.ItemID), boundCorrelation(t, ev.ItemID), true
		t.flags[FlagCallerPending] = true
		r.addSourceLocked(t, ev.EventID)
		r.callerByID[ev.ItemID] = t
	case realtime.Unavailable:
		// No caller text can ever arrive, so the turn is unavailable from
		// speech start, as it was before out-of-band transcription existed.
		if _, ok := r.callerByID[ev.ItemID]; ok {
			return
		}
		t := r.newTurnLocked(recording.SpeakerCaller, now)
		if t == nil {
			return
		}
		t.itemID, t.corr, t.unavail = boundItemID(t, ev.ItemID), boundCorrelation(t, ev.ItemID), true
		t.flags[FlagCallerUnavailable] = true
		r.addSourceLocked(t, ev.EventID)
		r.callerByID[ev.ItemID] = t
	}
}

// callerTextAllowedLocked reports whether caller text may be stored. On an
// out-of-band call only out-of-band text is accepted; on a native call all
// caller text is accepted (live_ws); with no caller transcript it is rejected.
func (r *Recorder) callerTextAllowedLocked(ev realtime.Event) bool {
	switch r.cfg.Caps.V1CallerTranscript() {
	case realtime.Native:
		return true
	case realtime.OutOfBand:
		return ev.OutOfBand
	}
	return false
}

// callerTextLocked joins caller text to the caller turn of its item. A turn is
// created when no speech start was seen for the item.
func (r *Recorder) callerTextLocked(ev realtime.Event, now int64) {
	if r.cfg.Caps.V1CallerTranscript() == realtime.OutOfBand {
		t := r.callerTurnLocked(ev, now)
		if t == nil {
			return
		}
		t.modelDerived = true
		t.flags[FlagCallerTextModelDerived] = true
		if ev.Kind == realtime.EventCallerTranscriptFinal {
			// Only a final (or a failure) settles the turn.
			t.pending = false
			delete(t.flags, FlagCallerPending)
			r.callerFinalLocked(t, ev)
			return
		}
		// A delta adds text but keeps the turn pending until its final or
		// failure arrives.
		if !t.failed {
			t.pending = true
			t.flags[FlagCallerPending] = true
		}
		if ev.ResponseID != "" {
			r.addSourceLocked(t, ev.ResponseID)
		}
		r.addFragmentLocked(t, ev, now)
		return
	}
	t := r.callerTurnLocked(ev, now)
	r.addFragmentLocked(t, ev, now)
}

// callerFinalLocked stores the final out-of-band text. It replaces the joined
// deltas, as a provider final does for agent text, and counts against the same
// text budget. The text is cut at a rune boundary and flagged if it is too long.
func (r *Recorder) callerFinalLocked(t *turn, ev realtime.Event) {
	r.callText -= t.doneBytes
	room := min(MaxTurnText, MaxCallText-r.callText)
	text := ev.Text
	if len(text) > room {
		text = truncateUTF8(text, max(room, 0))
		t.flags[FlagTruncatedText] = true
		r.flags[FlagTruncatedText] = true
	}
	t.doneBytes = len(text)
	r.callText += len(text)
	t.done = &text
	r.addSourceLocked(t, ev.EventID)
	if ev.ResponseID != "" {
		r.addSourceLocked(t, ev.ResponseID)
	}
	t.dirty = true
}

// callerFailedLocked records a failed out-of-band transcription on its caller
// turn. The provider event still goes to the timeline with its code.
func (r *Recorder) callerFailedLocked(ev realtime.Event, now int64) {
	r.providerEventLocked(ev)
	if ev.ItemID == "" || !ev.OutOfBand {
		return
	}
	t := r.callerTurnLocked(ev, now)
	if t == nil {
		return
	}
	t.pending = false
	delete(t.flags, FlagCallerPending)
	t.failed = true
	t.flags[FlagCallerFailed] = true
	if len(t.frags) > 0 {
		t.flags[FlagProviderIncomplete] = true
	}
	r.addSourceLocked(t, ev.EventID)
	t.dirty = true
}

func (r *Recorder) newTurnLocked(sp recording.Speaker, now int64) *turn {
	if len(r.turns) >= MaxTurns {
		// A memory ceiling far above any real call length. Turns are never
		// dropped below it.
		r.flags[FlagEventsDropped] = true
		return nil
	}
	t := &turn{seq: r.seqBase + r.nSeq + 1, speaker: sp, flags: map[string]bool{}, startMS: now, endMS: now, dirty: true}
	r.nSeq++
	r.turns = append(r.turns, t)
	return t
}

func (r *Recorder) touchLocal(t *turn, now int64) {
	if t.timed {
		return
	}
	if now > t.endMS {
		t.endMS = now
	}
	t.hasEnd = true
}

// addFragmentLocked inserts a text fragment. Fragments with provider timing
// are ordered on the provider timeline, as the Live guide directs; a fragment
// that arrives out of order is placed by its start_ms and flagged.
func (r *Recorder) addFragmentLocked(t *turn, ev realtime.Event, now int64) {
	if t == nil {
		return
	}
	if len(t.frags) >= MaxFragments || r.callFrags >= MaxCallFragments ||
		t.textBytes+len(ev.Text) > MaxTurnText || r.callText+len(ev.Text) > MaxCallText {
		r.flags[FlagEventsDropped] = true
		t.flags[FlagTruncatedText] = true
		t.dirty = true
		return
	}
	r.callText += len(ev.Text)
	r.callFrags++
	f := fragment{text: ev.Text, startMS: now, endMS: now}
	if ev.HasTiming {
		f.startMS, f.endMS, f.timed = ev.StartMS, ev.EndMS, true
	}
	if len(t.frags) == 0 && f.timed {
		t.timed, t.startMS, t.endMS = true, f.startMS, f.endMS
	}
	i := len(t.frags)
	if f.timed {
		i = sort.Search(len(t.frags), func(k int) bool { return t.frags[k].startMS > f.startMS })
		if i < len(t.frags) {
			t.flags[FlagOutOfOrder] = true
		}
	}
	t.frags = append(t.frags, fragment{})
	copy(t.frags[i+1:], t.frags[i:])
	t.frags[i] = f
	t.textBytes += len(ev.Text)
	if f.timed {
		t.startMS = min(t.startMS, f.startMS)
		t.endMS = max(t.endMS, f.endMS)
	} else if f.endMS > t.endMS {
		t.endMS = f.endMS
	}
	t.hasEnd = true
	r.addSourceLocked(t, ev.EventID)
	if t.wroteFin {
		t.flags[FlagCorrected] = true
	}
	t.dirty = true
}

// rememberEventLocked records a provider event id for replay detection. Only the
// most recent maxSeenEvents ids are kept, so a long call holds bounded memory.
// Live turns settle only at close, so a per-turn release cannot bound them
// (tracked in #117).
func (r *Recorder) rememberEventLocked(id string) {
	if len(r.seenOrder) >= maxSeenEvents {
		delete(r.seenEvents, r.seenOrder[0])
		r.seenOrder = r.seenOrder[1:]
	}
	r.seenEvents[id] = true
	r.seenOrder = append(r.seenOrder, id)
}

// addSourceLocked records a provider id a turn was built from. Past the per-turn
// or per-call bound the id is dropped and the call is flagged events_dropped.
func (r *Recorder) addSourceLocked(t *turn, id string) {
	if id == "" {
		return
	}
	if len(t.sources) >= MaxTurnSources || r.callSources >= MaxCallSources {
		r.flags[FlagEventsDropped] = true
		return
	}
	r.callSources++
	t.sources = append(t.sources, id)
}

// providerEventLocked keeps a provider event on the call timeline. Audio and
// transcript text are never copied into events: text lives in turns, audio in
// the recording.
func (r *Recorder) providerEventLocked(ev realtime.Event) {
	payload := map[string]any{"type": ev.RawType}
	if ev.ItemID != "" {
		payload["item_id"] = ev.ItemID
	}
	if ev.ResponseID != "" {
		payload["response_id"] = ev.ResponseID
	}
	if ev.HasTiming {
		payload["start_ms"] = ev.StartMS
	}
	switch ev.Kind {
	case realtime.EventTruncated:
		payload["audio_end_ms"] = ev.EndMS
	case realtime.EventResponseDone:
		payload["status"] = ev.ResponseStatus
		if ev.UsageTokens > 0 {
			payload["total_tokens"] = ev.UsageTokens
		}
	case realtime.EventDelegation:
		payload["delegation_id"] = ev.DelegationID
	case realtime.EventError, realtime.EventCallerTranscriptFailed:
		payload["code"] = ev.ErrCode
	case realtime.EventClosed:
		payload["reason"] = ev.CloseReason
		if ev.UsageSeconds > 0 {
			payload["usage_seconds"] = ev.UsageSeconds
		}
	}
	corr := ev.ResponseID
	if corr == "" {
		corr = ev.ItemID
	}
	r.addEventLocked(storage.CallEvent{
		Kind: "provider." + kindName(ev.Kind), Provider: string(r.cfg.Caps.Protocol),
		ProviderEventID: ev.EventID, CorrelationID: corr, PayloadJSON: mustJSON(payload),
	})
}

func (r *Recorder) addEventLocked(e storage.CallEvent) {
	if len(r.events) >= MaxEvents {
		r.flags[FlagEventsDropped] = true
		return
	}
	if e.Provider == "" {
		e.Provider = "internal"
	}
	e.OccurredAt = r.cfg.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	r.events = append(r.events, e)
}

// flush writes every changed turn as a new revision, then pending events.
// A turn is rewritten when its text, flags or timing changed, its finality
// changed, or final is set. A playout-only change is written once its playout
// has settled; unsettled playout is not written on every tick. With final set,
// every turn is written as final and the summary is appended.
func (r *Recorder) flush(ctx context.Context, final bool) error {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.mu.Lock()
	var revs []storage.TranscriptRevision
	var wrote []*turn
	var keys []string
	var seen []audio.ItemPlayout
	var entries []recording.Entry
	if final {
		// A caller turn still pending at close can no longer resolve: it is
		// unavailable, not a guess, and the call is flagged unresolved.
		for _, t := range r.turns {
			if t.speaker == recording.SpeakerCaller && t.pending {
				// Partial out-of-band text is kept, flagged as an incomplete
				// provider turn. With no text the turn is unavailable.
				t.pending = false
				delete(t.flags, FlagCallerPending)
				t.flags[FlagCallerUnresolved] = true
				if len(t.frags) == 0 {
					t.unavail = true
				} else {
					t.flags[FlagProviderIncomplete] = true
				}
				t.dirty = true
			}
		}
	}
	for _, t := range r.turns {
		// A periodic flush rebuilds only turns that can have changed: dirty
		// turns, and turns whose playout is not yet settled, because their
		// playout evidence may still change. A final turn with settled playout
		// and no change costs nothing. Only the final flush needs every entry,
		// for the summary.
		if !final && !t.dirty && t.wroteFin && !r.playoutOpenLocked(t) {
			continue
		}
		rev, entry, p := r.revisionLocked(t, final)
		entries = append(entries, entry)
		key := revKey(rev)
		finChange := rev.Finality == storage.FinalityFinal && !t.wroteFin
		// The written key covers text, flags, timing and playout, so a key
		// match means nothing changed.
		if key == t.lastWrite {
			t.dirty = false
			continue
		}
		// A periodic flush writes a turn with no text, flag or finality change
		// only once its playout has settled and differs from the last write.
		// Unsettled playout moves every tick and is held until it settles.
		if !final && !t.dirty && !finChange && (!playoutSettled(p) || p == t.playoutSeen) {
			t.dirty = false
			continue
		}
		if t.wroteFin {
			// A turn that already had a final revision changed again. Only a
			// text change is a correction; playout evidence alone is not.
			if rev.Text != t.finalText {
				t.flags[FlagCorrected] = true
			} else {
				t.flags[FlagPlayoutUpdated] = true
			}
			rev, _, p = r.revisionLocked(t, final)
			key = revKey(rev)
		}
		revs = append(revs, rev)
		wrote = append(wrote, t)
		keys = append(keys, key)
		seen = append(seen, p)
		t.dirty = false
	}
	if final {
		flags := r.summaryFlagsLocked(entries)
		r.events = append(r.events, storage.CallEvent{
			Kind: storage.TranscriptFinalizedKind, Provider: "internal",
			OccurredAt: r.cfg.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			PayloadJSON: mustJSON(map[string]any{
				"protocol": string(r.cfg.Caps.Protocol), "turns": len(r.turns), "quality_flags": flags,
				"two_party_complete": !contains(flags, FlagTwoPartyIncomplete),
			}),
		})
	}
	events := r.events
	r.events = nil
	r.mu.Unlock()

	if len(revs) == 0 && len(events) == 0 {
		return nil
	}
	err := r.cfg.Store.RecordTranscript(ctx, r.cfg.CallID, r.cfg.OwnerUserID, revs, events)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		for i, t := range wrote {
			t.lastWrite = keys[i]
			if revs[i].Finality == storage.FinalityFinal {
				t.wroteFin = true
				t.finalText = revs[i].Text
			}
			if t.speaker == recording.SpeakerAgent {
				t.playoutSeen = seen[i]
			}
		}
		return nil
	}
	if r.cfg.Logger != nil {
		r.cfg.Logger.Error("transcript write failed", "call_id", r.cfg.CallID, "error", err.Error())
	}
	// Nothing in the failed transaction was stored. Mark the turns for a
	// rewrite and put the events back for the next flush.
	for _, t := range wrote {
		t.dirty = true
	}
	r.events = append(events, r.events...)
	return err
}

// playoutSettled reports whether an agent turn's playout accounting is complete:
// every sent byte is estimated, discarded or unconfirmed.
func playoutSettled(p audio.ItemPlayout) bool {
	return p.Estimated+p.Discarded+p.Unconfirmed >= p.Sent
}

// playoutOpenLocked reports whether an agent turn's playout is still moving and
// should be rebuilt on a periodic flush. Unsettled playout is rebuilt; settled
// playout is rebuilt once, when it differs from the snapshot last written, so a
// settled change is persisted without waiting for Close. Caller turns are never
// open.
func (r *Recorder) playoutOpenLocked(t *turn) bool {
	if t.speaker != recording.SpeakerAgent {
		return false
	}
	p := r.playoutOfLocked(t)
	if !playoutSettled(p) {
		return true
	}
	return p != t.playoutSeen
}

// playoutOfLocked is the playout accounting of an agent turn's source. It is
// the zero value for other turns.
func (r *Recorder) playoutOfLocked(t *turn) audio.ItemPlayout {
	if t.speaker != recording.SpeakerAgent {
		return audio.ItemPlayout{}
	}
	p, _ := r.cfg.Playout(t.src)
	return p
}

// revisionLocked builds the current revision of t, its fidelity entry, and the
// playout accounting it was built from. The caller stores that playout as the
// turn's snapshot, so the revision and the snapshot come from one read.
func (r *Recorder) revisionLocked(t *turn, final bool) (storage.TranscriptRevision, recording.Entry, audio.ItemPlayout) {
	flags := map[string]bool{}
	for f := range t.flags {
		flags[f] = true
	}
	text := joinText(t.frags)
	if t.done != nil {
		text = *t.done
	}
	rev := storage.TranscriptRevision{
		Seq: t.seq, StartedMS: t.startMS, ProviderItemID: t.itemID, CorrelationID: t.corr,
		SourceIDs: append([]string(nil), t.sources...), TimingSource: storage.TimingLocalReceive,
	}
	if t.timed {
		rev.TimingSource = storage.TimingProvider
	} else {
		flags[FlagTimingLocal] = true
	}
	if t.hasEnd {
		end := t.endMS
		rev.EndedMS = &end
	}
	var entry recording.Entry
	var played audio.ItemPlayout
	settled := true
	switch t.speaker {
	case recording.SpeakerCaller:
		rev.Speaker = storage.SpeakerCallee
		switch {
		case t.unavail:
			entry = recording.CallerEntry("", false, t.sources, t.timed, t.startMS, t.endMS)
		case t.modelDerived:
			// Out-of-band text is model-derived and approximate: provider_asr
			// fidelity with an explicit model-derived flag, never a claim of
			// provider ASR. Empty text stays empty and is flagged.
			entry = recording.CallerEntry(text, true, t.sources, t.timed, t.startMS, t.endMS)
			if text == "" {
				flags[FlagCallerEmpty] = true
			}
			flags[FlagCallerTextModelDerived] = true
		case t.failed && text != "":
			// A transcription that failed after partial text keeps the partial
			// text, flagged as incomplete. It is model-derived text too.
			entry = recording.CallerEntry(text, true, t.sources, t.timed, t.startMS, t.endMS)
			flags[FlagCallerTextModelDerived] = true
		case t.pending, t.failed:
			entry = recording.CallerEntry("", false, t.sources, t.timed, t.startMS, t.endMS)
		default:
			entry = recording.CallerEntry(text, true, t.sources, t.timed, t.startMS, t.endMS)
			if r.live {
				flags[FlagGroupedLocally] = true
			}
		}
	case recording.SpeakerAgent:
		rev.Speaker = storage.SpeakerAssistant
		p, _ := r.cfg.Playout(t.src)
		played = p
		settled = playoutSettled(p)
		e, err := recording.AgentEntry(text, t.sources, p)
		if err != nil {
			// Audio was sent with no transcript: there is no text to show.
			e = recording.Entry{Speaker: recording.SpeakerAgent, Fidelity: recording.FidelityUnavailable, Generated: p.Sent}
			flags[FlagAgentMissing] = true
		}
		if r.live {
			// Live agent audio has no item ID: its utterance number is a local
			// guess. Text with no sent audio is unconfirmed, not a claim that
			// it was never played.
			flags[FlagPlayoutAttribution] = true
			if e.Fidelity == recording.FidelityNotPlayed && text != "" {
				e.Fidelity = recording.FidelityUnconfirmed
			}
		}
		entry = e
		gen, playedMS := e.Generated.Milliseconds(), e.Played.Milliseconds()
		rev.GeneratedMS, rev.PlayedMS, rev.PlayedIsEstimate = &gen, &playedMS, e.PlayedIsEstimate
		switch e.Fidelity {
		case recording.FidelityInterrupted:
			flags[FlagAgentUnplayed] = true
		case recording.FidelityUnconfirmed:
			flags[FlagPlayoutUnconfirmed] = true
		case recording.FidelityNotPlayed:
			flags[FlagAgentNotPlayed] = true
		}
		if r.live {
			flags[FlagGroupedLocally] = true
		} else if final && t.done == nil && len(t.frags) > 0 {
			flags[FlagProviderIncomplete] = true
		}
	}
	rev.Text, rev.Fidelity = entry.Text, string(entry.Fidelity)
	rev.Finality = storage.FinalityPartial
	switch {
	case final, t.unavail:
		rev.Finality = storage.FinalityFinal
	case !r.live && t.speaker == recording.SpeakerAgent && t.done != nil && settled:
		rev.Finality = storage.FinalityFinal
	}
	rev.Quality = sortedKeys(flags)
	return rev, entry, played
}

func (r *Recorder) summaryFlagsLocked(entries []recording.Entry) []string {
	flags := map[string]bool{}
	for f := range r.flags {
		flags[f] = true
	}
	for _, t := range r.turns {
		for f := range t.flags {
			flags[f] = true
		}
	}
	for _, e := range entries {
		switch e.Fidelity {
		case recording.FidelityInterrupted:
			flags[FlagAgentUnplayed] = true
		case recording.FidelityUnconfirmed:
			flags[FlagPlayoutUnconfirmed] = true
		case recording.FidelityNotPlayed:
			flags[FlagAgentNotPlayed] = true
		case recording.FidelityUnavailable:
			if e.Speaker == recording.SpeakerAgent {
				flags[FlagAgentMissing] = true
			}
		}
	}
	if r.cfg.Caps.V1CallerTranscript() == realtime.Unavailable {
		flags[FlagCallerUnavailable] = true
	}
	if flags[FlagCallerUnavailable] || flags[FlagCallerFailed] || flags[FlagCallerUnresolved] || flags[FlagCallerPending] ||
		flags[FlagEventsDropped] || flags[FlagAgentMissing] || recording.Complete(entries) != nil {
		flags[FlagTwoPartyIncomplete] = true
	}
	return sortedKeys(flags)
}

func (r *Recorder) summaryJSON(extra map[string]any) string {
	if _, ok := extra["quality_flags"]; !ok {
		flags := map[string]bool{}
		for f := range r.flags {
			flags[f] = true
		}
		extra["quality_flags"] = sortedKeys(flags)
	}
	return mustJSON(extra)
}

func revKey(r storage.TranscriptRevision) string {
	b, _ := json.Marshal(r)
	return string(b)
}

func joinText(frags []fragment) string {
	n := 0
	for _, f := range frags {
		n += len(f.text)
	}
	b := make([]byte, 0, n)
	for _, f := range frags {
		b = append(b, f.text...)
	}
	return string(b)
}

// boundItemID returns id for storage in provider_item_id. An oversized id is
// not stored, and the turn is flagged provider_id_invalid.
func boundItemID(t *turn, id string) string {
	if len(id) > maxProviderID {
		t.flags[FlagProviderIDInvalid] = true
		t.dirty = true
		return ""
	}
	return id
}

// boundCorrelation returns corr for storage in transcript_turns.correlation_id,
// which holds 1 to maxProviderID bytes. A longer identifier is dropped and the
// turn is flagged, so one oversized id cannot fail the whole transaction.
func boundCorrelation(t *turn, corr string) string {
	if len(corr) > maxProviderID {
		t.flags[FlagProviderIDInvalid] = true
		t.dirty = true
		return ""
	}
	return corr
}

func correlationOf(src audio.SourceRef) string {
	if src.ItemID != "" {
		return src.ItemID
	}
	if src.Utterance > 0 {
		return "utterance:" + strconv.FormatUint(src.Utterance, 10)
	}
	return ""
}

func kindName(k realtime.EventKind) string {
	switch k {
	case realtime.EventSpeechStarted:
		return "speech_started"
	case realtime.EventTruncated:
		return "truncated"
	case realtime.EventResponseDone:
		return "response_done"
	case realtime.EventDelegation:
		return "delegation"
	case realtime.EventError:
		return "error"
	case realtime.EventClosed:
		return "closed"
	case realtime.EventCallerTranscriptFailed:
		return "caller_transcript_failed"
	}
	return "other"
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
