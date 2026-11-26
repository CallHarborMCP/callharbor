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
)

// Bounds on what one call may hold in memory and in storage. Past a bound the
// excess is dropped and flagged, never silently cut.
const (
	MaxTurns       = 2000
	MaxTurnText    = 32 << 10
	MaxFragments   = 4096
	MaxEvents      = 4000
	FlushInterval  = time.Second
	CallerTurnGap  = 2 * time.Second
	finalizeBudget = 10 * time.Second
)

// Store persists transcript revisions and call events.
type Store interface {
	RecordTranscript(ctx context.Context, callID, ownerUserID string, turns []storage.TranscriptRevision, events []storage.CallEvent) error
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
	done      *string
	sources   []string
	timed     bool
	startMS   int64
	endMS     int64
	hasEnd    bool
	flags     map[string]bool
	dirty     bool
	wroteFin  bool
	unavail   bool
	lastWrite string
	textBytes int
}

// Recorder collects the events of one call. Observe and OnCut may be called
// from any goroutine. It is not reusable after Close.
type Recorder struct {
	cfg   Config
	start time.Time
	live  bool

	mu         sync.Mutex
	turns      []*turn
	agentBySrc map[audio.SourceRef]*turn
	callerByID map[string]*turn
	seenEvents map[string]bool
	events     []storage.CallEvent
	nEvents    int
	nFrags     int
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
	r := &Recorder{
		cfg:        cfg,
		start:      cfg.Now(),
		live:       cfg.Caps.Protocol == realtime.ProtocolLive,
		agentBySrc: map[audio.SourceRef]*turn{},
		callerByID: map[string]*turn{},
		seenEvents: map[string]bool{},
		flags:      map[string]bool{},
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	if cfg.Caps.V1CallerTranscript() == realtime.Unavailable {
		r.flags[FlagCallerUnavailable] = true
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
	if ev.EventID != "" && (ev.Kind == realtime.EventAgentTranscriptDelta || ev.Kind == realtime.EventAgentTranscriptDone ||
		ev.Kind == realtime.EventCallerTranscriptDelta || ev.Kind == realtime.EventCallerTranscriptFinal) {
		if r.seenEvents[ev.EventID] {
			// A replayed provider event is recorded once.
			return
		}
		if len(r.seenEvents) >= MaxFragments+MaxTurns {
			r.flags[FlagEventsDropped] = true
			return
		}
		r.seenEvents[ev.EventID] = true
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
		if r.cfg.Caps.V1CallerTranscript() == realtime.Unavailable {
			// Separately billed transcription is never enabled. Text that
			// arrives anyway is outside the contract and is not stored.
			r.flags[FlagUnexpectedCaller] = true
			return
		}
		t := r.callerTurnLocked(ev, now)
		r.addFragmentLocked(t, ev, now)
	case realtime.EventSpeechStarted:
		r.callerSpeechLocked(ev, now)
	case realtime.EventTruncated, realtime.EventResponseDone, realtime.EventDelegation,
		realtime.EventError, realtime.EventClosed, realtime.EventCallerTranscriptFailed:
		r.providerEventLocked(ev)
	}
}

// OnCut records a barge-in cut with its cause. callerTurn is the local caller
// turn that caused it, or zero for a provider speech-start barge-in.
func (r *Recorder) OnCut(c telnyx.Cut, callerTurn uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	payload := map[string]any{
		"audible": c.Audible, "played_ms": c.Played.Milliseconds(), "sent_ms": c.Sent.Milliseconds(),
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
	t.src, t.itemID, t.corr = src, src.ItemID, correlationOf(src)
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
			t.itemID, t.corr = ev.ItemID, ev.ItemID
			r.callerByID[ev.ItemID] = t
		}
		return t
	}
	if last := r.lastCallerLocked(); last != nil && ev.HasTiming && last.timed {
		gap := ev.StartMS - last.endMS
		if gap <= CallerTurnGap.Milliseconds() && !r.agentStartedBetweenLocked(last.startMS, ev.StartMS) {
			return last
		}
	}
	t := r.newTurnLocked(recording.SpeakerCaller, now)
	if t != nil {
		t.corr = "caller:" + strconv.Itoa(t.seq)
	}
	return t
}

func (r *Recorder) lastCallerLocked() *turn {
	for i := len(r.turns) - 1; i >= 0; i-- {
		if r.turns[i].speaker == recording.SpeakerCaller && !r.turns[i].unavail {
			return r.turns[i]
		}
	}
	return nil
}

// agentStartedBetweenLocked reports whether an agent fragment starts on the
// provider timeline after the caller turn began and before at. A late caller
// fragment from before the agent replied still joins its turn.
func (r *Recorder) agentStartedBetweenLocked(callerStart, at int64) bool {
	for _, t := range r.turns {
		if t.speaker != recording.SpeakerAgent {
			continue
		}
		for _, f := range t.frags {
			if f.timed && f.startMS > callerStart && f.startMS < at {
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
	if r.cfg.Caps.V1CallerTranscript() != realtime.Unavailable || ev.ItemID == "" {
		return
	}
	if _, ok := r.callerByID[ev.ItemID]; ok {
		return
	}
	t := r.newTurnLocked(recording.SpeakerCaller, now)
	if t == nil {
		return
	}
	t.itemID, t.corr, t.unavail = ev.ItemID, ev.ItemID, true
	t.flags[FlagCallerUnavailable] = true
	r.addSourceLocked(t, ev.EventID)
	r.callerByID[ev.ItemID] = t
}

func (r *Recorder) newTurnLocked(sp recording.Speaker, now int64) *turn {
	if len(r.turns) >= MaxTurns {
		r.flags[FlagEventsDropped] = true
		return nil
	}
	t := &turn{seq: len(r.turns) + 1, speaker: sp, flags: map[string]bool{}, startMS: now, endMS: now, dirty: true}
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
	if r.nFrags >= MaxFragments || t.textBytes+len(ev.Text) > MaxTurnText {
		r.flags[FlagEventsDropped] = true
		t.flags[FlagTruncatedText] = true
		t.dirty = true
		return
	}
	r.nFrags++
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

func (r *Recorder) addSourceLocked(t *turn, id string) {
	if id == "" || len(t.sources) >= 256 {
		return
	}
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
	if r.nEvents >= MaxEvents {
		r.flags[FlagEventsDropped] = true
		return
	}
	if e.Provider == "" {
		e.Provider = "internal"
	}
	e.OccurredAt = r.cfg.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	r.nEvents++
	r.events = append(r.events, e)
}

// flush writes every changed turn as a new revision, then pending events.
// A turn is rewritten when its text or flags changed, its finality changed or
// final is set; playout progress alone waits for one of those. With final set,
// every turn is written as final and the summary is appended.
func (r *Recorder) flush(ctx context.Context, final bool) error {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()
	r.mu.Lock()
	var revs []storage.TranscriptRevision
	var wrote []*turn
	var keys []string
	var entries []recording.Entry
	for _, t := range r.turns {
		rev, entry := r.revisionLocked(t, final)
		entries = append(entries, entry)
		key := revKey(rev)
		finChange := rev.Finality == storage.FinalityFinal && !t.wroteFin
		if key == t.lastWrite || (!t.dirty && !finChange && !final) {
			t.dirty = false
			continue
		}
		if t.wroteFin && !t.flags[FlagCorrected] {
			// A turn that already had a final revision changed again.
			t.flags[FlagCorrected] = true
			rev, _ = r.revisionLocked(t, final)
			key = revKey(rev)
		}
		revs = append(revs, rev)
		wrote = append(wrote, t)
		keys = append(keys, key)
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

// revisionLocked builds the current revision of t and its fidelity entry.
func (r *Recorder) revisionLocked(t *turn, final bool) (storage.TranscriptRevision, recording.Entry) {
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
	settled := true
	switch t.speaker {
	case recording.SpeakerCaller:
		rev.Speaker = storage.SpeakerCallee
		if t.unavail {
			entry = recording.CallerEntry("", false, t.sources, t.timed, t.startMS, t.endMS)
		} else {
			entry = recording.CallerEntry(text, true, t.sources, t.timed, t.startMS, t.endMS)
			if r.live {
				flags[FlagGroupedLocally] = true
			}
		}
	case recording.SpeakerAgent:
		rev.Speaker = storage.SpeakerAssistant
		p, _ := r.cfg.Playout(t.src)
		settled = p.Estimated+p.Discarded+p.Unconfirmed >= p.Sent
		e, err := recording.AgentEntry(text, t.sources, p)
		if err != nil {
			// Audio was sent with no transcript: there is no text to show.
			e = recording.Entry{Speaker: recording.SpeakerAgent, Fidelity: recording.FidelityUnavailable, Generated: p.Sent}
			flags[FlagAgentMissing] = true
		}
		entry = e
		gen, played := e.Generated.Milliseconds(), e.Played.Milliseconds()
		rev.GeneratedMS, rev.PlayedMS, rev.PlayedIsEstimate = &gen, &played, e.PlayedIsEstimate
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
	return rev, entry
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
	if flags[FlagCallerUnavailable] || flags[FlagEventsDropped] || flags[FlagAgentMissing] || recording.Complete(entries) != nil {
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
