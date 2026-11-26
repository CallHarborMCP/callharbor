package audio

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Played-audio accounting for a telephony leg whose only playback evidence is
// an echoed marker (Telnyx "mark"). Telnyx echoes a mark when the media queued
// before it finishes, but ALSO immediately when nothing is queued and in bulk
// after "clear", with no field distinguishing the cases. The ledger therefore
// classifies every echo against its own send history instead of trusting it.

// SourceRef ties an outbound chunk to the voice-provider output it came from.
type SourceRef struct {
	// ItemID is the provider output identifier (Realtime item_id, or a
	// locally assigned utterance id for protocols without one).
	ItemID       string
	ContentIndex int
}

// MarkOutcome classifies an echoed mark.
type MarkOutcome int

const (
	// MarkPlayed: the echo arrived no earlier than real-time playout allows
	// and no clear was issued while it was pending.
	MarkPlayed MarkOutcome = iota + 1
	// MarkFlushed: the chunk was discarded by a clear; it is not played audio.
	MarkFlushed
	// MarkEarly: echoed before its audio could have finished at real-time
	// rate (e.g. Telnyx returned it because its queue was empty). Not
	// accepted as playback evidence.
	MarkEarly
)

func (o MarkOutcome) String() string {
	switch o {
	case MarkPlayed:
		return "played"
	case MarkFlushed:
		return "flushed"
	case MarkEarly:
		return "early"
	}
	return "unknown"
}

var (
	ErrUnknownMark     = errors.New("audio: echo for unknown mark")
	ErrMarkOutOfOrder  = errors.New("audio: mark echoed out of FIFO order")
	ErrPlayoutTimedOut = errors.New("audio: playout confirmation missing")
)

// ItemPlayout is the per-source accounting result.
type ItemPlayout struct {
	Source SourceRef
	// Sent is audio handed to the telephony leg.
	Sent time.Duration
	// Confirmed is audio whose following mark echoed as MarkPlayed.
	Confirmed time.Duration
	// Estimated is the best estimate of audible audio: Confirmed plus the
	// real-time elapsed portion of chunks interrupted by a clear. It is an
	// approximation and always within [Confirmed, Sent].
	Estimated time.Duration
	// Discarded is audio removed by clear before it could have played.
	Discarded time.Duration
	// Unconfirmed is audio whose mark never echoed within the grace period
	// or echoed too early.
	Unconfirmed time.Duration
	// Interrupted is true if a clear cut this item.
	Interrupted bool
}

type chunk struct {
	mark      string
	seq       uint64
	src       SourceRef
	dur       time.Duration
	playStart time.Time
	cleared   bool
	expired   bool
}

// seqRange is an inclusive run of trimmed mark sequence numbers.
type seqRange struct{ lo, hi uint64 }

// PlayoutLedger tracks chunks sent to one telephony leg. It is not safe for
// concurrent use; the owning media loop serialises access.
type PlayoutLedger struct {
	format    Format
	tolerance time.Duration
	grace     time.Duration
	seq       uint64
	queueEnd  time.Time
	pending   []*chunk
	// trimmed holds the sequence numbers of expired chunks that trimExpired
	// dropped from pending, as sorted, disjoint inclusive ranges. An echo for
	// one of them is a genuine late echo, not a forged or unknown mark, so it
	// is accepted without touching the accounting, which was settled at expiry.
	trimmed []seqRange
	items   map[SourceRef]*ItemPlayout
	order   []SourceRef
}

// NewPlayoutLedger creates a ledger. tolerance bounds how much earlier than
// real-time an echo may arrive and still count as played (jitter); grace is
// how long after expected completion an echo may be outstanding.
func NewPlayoutLedger(f Format, tolerance, grace time.Duration) (*PlayoutLedger, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if tolerance < 0 || grace <= 0 {
		return nil, fmt.Errorf("audio: invalid ledger timing tolerance=%v grace=%v", tolerance, grace)
	}
	return &PlayoutLedger{format: f, tolerance: tolerance, grace: grace, items: map[SourceRef]*ItemPlayout{}}, nil
}

func (l *PlayoutLedger) item(src SourceRef) *ItemPlayout {
	it, ok := l.items[src]
	if !ok {
		it = &ItemPlayout{Source: src}
		l.items[src] = it
		l.order = append(l.order, src)
	}
	return it
}

// Send records n encoded bytes handed to the leg at now and returns the mark
// name the caller must send immediately after the media.
func (l *PlayoutLedger) Send(src SourceRef, n int, now time.Time) (string, error) {
	d, err := l.format.Duration(n)
	if err != nil {
		return "", err
	}
	if d <= 0 {
		return "", fmt.Errorf("audio: empty playout chunk")
	}
	start := now
	if l.queueEnd.After(start) {
		start = l.queueEnd
	}
	l.queueEnd = start.Add(d)
	l.seq++
	c := &chunk{mark: "ch-" + strconv.FormatUint(l.seq, 10), seq: l.seq, src: src, dur: d, playStart: start}
	l.pending = append(l.pending, c)
	l.item(src).Sent += d
	return c.mark, nil
}

// Echo classifies an echoed mark received at now.
func (l *PlayoutLedger) Echo(name string, now time.Time) (MarkOutcome, error) {
	idx := -1
	for i, c := range l.pending {
		if c.mark == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		// Not pending: either trimmed (a genuine late echo, see trimExpired) or
		// unknown. The trimmed echo is accepted once, as MarkEarly, and its
		// accounting is not touched again: Unconfirmed was recorded at expiry.
		if seq, ok := parseMarkSeq(name); ok && l.takeTrimmed(seq) {
			return MarkEarly, nil
		}
		return 0, fmt.Errorf("%w %q", ErrUnknownMark, name)
	}
	c := l.pending[idx]
	if c.cleared {
		l.remove(idx)
		return MarkFlushed, nil
	}
	for _, p := range l.pending[:idx] {
		if !p.cleared && !p.expired {
			return 0, fmt.Errorf("%w: %q before %q", ErrMarkOutOfOrder, name, p.mark)
		}
	}
	l.remove(idx)
	it := l.item(c.src)
	if c.expired {
		// Late echo after timeout: keep it unconfirmed; evidence already failed.
		return MarkEarly, nil
	}
	if now.Before(c.playStart.Add(c.dur).Add(-l.tolerance)) {
		it.Unconfirmed += c.dur
		return MarkEarly, nil
	}
	it.Confirmed += c.dur
	it.Estimated += c.dur
	return MarkPlayed, nil
}

func (l *PlayoutLedger) remove(i int) {
	copy(l.pending[i:], l.pending[i+1:])
	l.pending[len(l.pending)-1] = nil
	l.pending = l.pending[:len(l.pending)-1]
}

// Clear records that a clear command was sent at now. Pending chunks that
// had not finished by now are split into an estimated-played prefix and a
// discarded remainder; their eventual echoes are classified as MarkFlushed.
// Chunks that should already have finished keep awaiting their in-flight
// echo, which still confirms or fails them individually. It reports whether
// any chunk was still playing at now, i.e. whether the clear cut audible audio.
func (l *PlayoutLedger) Clear(now time.Time) (cut bool) {
	for _, c := range l.pending {
		if c.cleared || c.expired {
			continue
		}
		if !now.Before(c.playStart.Add(c.dur)) {
			continue
		}
		c.cleared = true
		cut = true
		it := l.item(c.src)
		played := now.Sub(c.playStart)
		if played < 0 {
			played = 0
		}
		if played > c.dur {
			played = c.dur
		}
		it.Estimated += played
		it.Discarded += c.dur - played
		it.Interrupted = true
	}
	l.queueEnd = now
	return cut
}

// QueueEnd is the media time at which everything sent so far finishes playing
// at real-time rate. A time at or before now means nothing is queued.
func (l *PlayoutLedger) QueueEnd() time.Time { return l.queueEnd }

// Expire marks chunks whose echo is overdue at now and returns
// ErrPlayoutTimedOut if any were found. Overdue chunks count as Unconfirmed,
// once, when they are marked expired. An expired chunk stays in the pending
// list so a late echo is still classified in FIFO order; the list is bounded
// by MaxExpiredPending, see trimExpired.
func (l *PlayoutLedger) Expire(now time.Time) error {
	var n int
	for _, c := range l.pending {
		if c.cleared || c.expired {
			continue
		}
		if now.After(c.playStart.Add(c.dur).Add(l.grace)) {
			c.expired = true
			l.item(c.src).Unconfirmed += c.dur
			n++
		}
	}
	l.trimExpired()
	if n > 0 {
		return fmt.Errorf("%w: %d chunk(s)", ErrPlayoutTimedOut, n)
	}
	return nil
}

// MaxExpiredPending bounds the expired chunks kept for late echoes on one leg.
// A stalled leg would otherwise grow the pending list for as long as it stalls.
// Past the bound the oldest expired chunks are dropped. Their Unconfirmed time
// was recorded at expiry, so the accounting is unchanged; only a very late echo
// for a dropped chunk is no longer recognised, and it is reported as unknown.
const MaxExpiredPending = 256

// trimExpired drops the oldest expired chunks once more than MaxExpiredPending
// of them are waiting. Unexpired chunks are never dropped. Each dropped
// sequence is recorded in trimmed, so its late echo is still classified.
func (l *PlayoutLedger) trimExpired() {
	expired := 0
	for _, c := range l.pending {
		if c.expired {
			expired++
		}
	}
	excess := expired - MaxExpiredPending
	if excess <= 0 {
		return
	}
	kept := l.pending[:0]
	for _, c := range l.pending {
		if c.expired && excess > 0 {
			excess--
			l.addTrimmed(c.seq)
			continue
		}
		kept = append(kept, c)
	}
	for i := len(kept); i < len(l.pending); i++ {
		l.pending[i] = nil
	}
	l.pending = kept
}

// addTrimmed records one dropped sequence, merging it into the ranges.
// Trimming is oldest-first, so sequences arrive in ascending order and the
// common case extends the last range.
func (l *PlayoutLedger) addTrimmed(seq uint64) {
	if n := len(l.trimmed); n > 0 && l.trimmed[n-1].hi+1 == seq {
		l.trimmed[n-1].hi = seq
		return
	}
	l.trimmed = append(l.trimmed, seqRange{lo: seq, hi: seq})
}

// takeTrimmed reports whether seq is a trimmed sequence and removes it, so a
// second echo for the same mark is not classified again.
func (l *PlayoutLedger) takeTrimmed(seq uint64) bool {
	for i, r := range l.trimmed {
		if seq < r.lo || seq > r.hi {
			continue
		}
		switch {
		case r.lo == r.hi:
			l.trimmed = append(l.trimmed[:i], l.trimmed[i+1:]...)
		case seq == r.lo:
			l.trimmed[i].lo++
		case seq == r.hi:
			l.trimmed[i].hi--
		default:
			// Split the range around seq.
			tail := seqRange{lo: seq + 1, hi: r.hi}
			l.trimmed[i].hi = seq - 1
			l.trimmed = append(l.trimmed[:i+1], append([]seqRange{tail}, l.trimmed[i+1:]...)...)
		}
		return true
	}
	return false
}

// parseMarkSeq extracts the sequence number from a mark name issued by Send.
func parseMarkSeq(name string) (uint64, bool) {
	const prefix = "ch-"
	if len(name) <= len(prefix) || name[:len(prefix)] != prefix {
		return 0, false
	}
	seq, err := strconv.ParseUint(name[len(prefix):], 10, 64)
	if err != nil || seq == 0 {
		return 0, false
	}
	return seq, true
}

// Pending reports chunks still awaiting an echo.
func (l *PlayoutLedger) Pending() int { return len(l.pending) }

// PendingLive counts marks still awaiting an echo that have not expired. An
// expired mark is kept only so a late echo can be classified; it no longer
// represents audio the stream is waiting on, so it does not count toward a
// playout cap.
func (l *PlayoutLedger) PendingLive() int {
	n := 0
	for _, c := range l.pending {
		if !c.expired {
			n++
		}
	}
	return n
}

// Position reports the audible position of one source at now, for a truncate
// at a cut. It is Estimated plus the full duration of chunks that finished at
// real-rate but whose echo has not arrived yet, capped at Sent. Estimated alone
// lags by those chunks, because only an echo moves a chunk into Confirmed.
//
// This is derived from the send-time queue, so it can overstate by network and
// jitter latency: a chunk counted as finished may still be in transit to the
// caller. It is never more than Sent.
func (l *PlayoutLedger) Position(src SourceRef, now time.Time) time.Duration {
	it, ok := l.items[src]
	if !ok {
		return 0
	}
	pos := it.Estimated
	for _, c := range l.pending {
		if c.src != src || c.cleared || c.expired {
			continue
		}
		if !now.Before(c.playStart.Add(c.dur)) {
			pos += c.dur
		}
	}
	if pos > it.Sent {
		pos = it.Sent
	}
	return pos
}

// Item returns the accounting for one source.
func (l *PlayoutLedger) Item(src SourceRef) (ItemPlayout, bool) {
	it, ok := l.items[src]
	if !ok {
		return ItemPlayout{}, false
	}
	return *it, true
}

// Items returns all sources in first-sent order.
func (l *PlayoutLedger) Items() []ItemPlayout {
	out := make([]ItemPlayout, 0, len(l.order))
	for _, s := range l.order {
		out = append(out, *l.items[s])
	}
	return out
}
