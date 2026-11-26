package audio

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

func newTestLedger(t *testing.T) *PlayoutLedger {
	t.Helper()
	l, err := NewPlayoutLedger(Format{Encoding: PCMU, SampleRate: 8000}, 5*time.Millisecond, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func mustSend(t *testing.T, l *PlayoutLedger, src SourceRef, n int, now time.Time) string {
	t.Helper()
	m, err := l.Send(src, n, now)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPlayoutLedgerPlayedQueuesBackToBack(t *testing.T) {
	l := newTestLedger(t)
	src := SourceRef{ItemID: "a"}
	m1 := mustSend(t, l, src, 160, t0)
	m2 := mustSend(t, l, src, 160, t0)
	if m1 == m2 || l.Pending() != 2 {
		t.Fatalf("marks %q %q pending %d", m1, m2, l.Pending())
	}
	if o, err := l.Echo(m1, t0.Add(20*time.Millisecond)); err != nil || o != MarkPlayed {
		t.Fatalf("m1 %v %v", o, err)
	}
	if o, err := l.Echo(m2, t0.Add(37*time.Millisecond)); err != nil || o != MarkPlayed {
		t.Fatalf("m2 within tolerance %v %v", o, err)
	}
	it, ok := l.Item(src)
	if !ok || it.Sent != 40*time.Millisecond || it.Confirmed != it.Sent || it.Estimated != it.Sent || it.Interrupted {
		t.Fatalf("%+v", it)
	}
	if _, ok := l.Item(SourceRef{ItemID: "missing"}); ok {
		t.Fatal("unknown source reported")
	}
}

func TestPlayoutLedgerEarlyEcho(t *testing.T) {
	l := newTestLedger(t)
	src := SourceRef{ItemID: "a"}
	m := mustSend(t, l, src, 160, t0)
	if o, err := l.Echo(m, t0); err != nil || o != MarkEarly {
		t.Fatalf("%v %v", o, err)
	}
	it, _ := l.Item(src)
	if it.Confirmed != 0 || it.Unconfirmed != 20*time.Millisecond {
		t.Fatalf("%+v", it)
	}
}

func TestPlayoutLedgerClearSplitsAndFlushes(t *testing.T) {
	l := newTestLedger(t)
	a, b := SourceRef{ItemID: "a"}, SourceRef{ItemID: "b", ContentIndex: 1}
	m1 := mustSend(t, l, a, 160, t0)
	m2 := mustSend(t, l, a, 160, t0)
	m3 := mustSend(t, l, b, 160, t0)
	l.Clear(t0.Add(25 * time.Millisecond))
	l.Clear(t0.Add(26 * time.Millisecond))
	if o, err := l.Echo(m3, t0.Add(26*time.Millisecond)); err != nil || o != MarkFlushed {
		t.Fatalf("m3 %v %v", o, err)
	}
	if o, err := l.Echo(m1, t0.Add(26*time.Millisecond)); err != nil || o != MarkPlayed {
		t.Fatalf("m1 finished before clear: %v %v", o, err)
	}
	if o, err := l.Echo(m2, t0.Add(26*time.Millisecond)); err != nil || o != MarkFlushed {
		t.Fatalf("m2 %v %v", o, err)
	}
	items := l.Items()
	if len(items) != 2 || items[0].Source != a || items[1].Source != b {
		t.Fatalf("order %+v", items)
	}
	ia, ib := items[0], items[1]
	if !ia.Interrupted || ia.Confirmed != 20*time.Millisecond || ia.Estimated != 25*time.Millisecond || ia.Discarded != 15*time.Millisecond {
		t.Fatalf("a %+v", ia)
	}
	if !ib.Interrupted || ib.Estimated != 0 || ib.Discarded != 20*time.Millisecond {
		t.Fatalf("b %+v", ib)
	}
	m4 := mustSend(t, l, a, 160, t0.Add(30*time.Millisecond))
	if o, err := l.Echo(m4, t0.Add(50*time.Millisecond)); err != nil || o != MarkPlayed {
		t.Fatalf("queue restarts at clear time: %v %v", o, err)
	}
}

func TestPlayoutLedgerExpireAndLateEcho(t *testing.T) {
	l := newTestLedger(t)
	src := SourceRef{ItemID: "a"}
	m1 := mustSend(t, l, src, 160, t0)
	m2 := mustSend(t, l, src, 160, t0)
	if err := l.Expire(t0.Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("within grace: %v", err)
	}
	if err := l.Expire(t0.Add(130 * time.Millisecond)); !errors.Is(err, ErrPlayoutTimedOut) {
		t.Fatalf("got %v", err)
	}
	if err := l.Expire(t0.Add(time.Second)); !errors.Is(err, ErrPlayoutTimedOut) {
		t.Fatalf("m2 overdue: %v", err)
	}
	if err := l.Expire(t0.Add(2 * time.Second)); err != nil {
		t.Fatalf("already expired chunks must not be recounted: %v", err)
	}
	l.Clear(t0.Add(2 * time.Second))
	if o, err := l.Echo(m2, t0.Add(2*time.Second)); err != nil || o != MarkEarly {
		t.Fatalf("expired predecessor must not block: %v %v", o, err)
	}
	if o, err := l.Echo(m1, t0.Add(2*time.Second)); err != nil || o != MarkEarly {
		t.Fatalf("late echo stays unconfirmed: %v %v", o, err)
	}
	it, _ := l.Item(src)
	if it.Confirmed != 0 || it.Unconfirmed != 40*time.Millisecond || l.Pending() != 0 {
		t.Fatalf("%+v pending %d", it, l.Pending())
	}
}

func TestPlayoutLedgerSendErrors(t *testing.T) {
	l := newTestLedger(t)
	if _, err := l.Send(SourceRef{}, 0, t0); err == nil {
		t.Fatal("empty chunk accepted")
	}
	l16, err := NewPlayoutLedger(Format{Encoding: L16, SampleRate: 16000}, 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l16.Send(SourceRef{}, 3, t0); err == nil {
		t.Fatal("partial L16 sample accepted")
	}
}

func TestMarkOutcomeString(t *testing.T) {
	for o, want := range map[MarkOutcome]string{MarkPlayed: "played", MarkFlushed: "flushed", MarkEarly: "early", 0: "unknown"} {
		if got := o.String(); got != want {
			t.Errorf("%d: %q want %q", o, got, want)
		}
	}
}

func TestCodecErrorsAndHelpers(t *testing.T) {
	bad := Format{Encoding: "opus", SampleRate: 48000}
	if _, err := Decode(nil, bad, []byte{0}); err == nil {
		t.Error("decode unknown encoding")
	}
	if _, err := Encode(nil, bad, []int16{0}); err == nil {
		t.Error("encode unknown encoding")
	}
	if _, err := Decode(nil, Format{Encoding: L16, SampleRate: 16000}, []byte{1}); err == nil {
		t.Error("odd L16 accepted")
	}
	for _, f := range []Format{{Encoding: PCMU, SampleRate: 8000}, {Encoding: PCMA, SampleRate: 8000}} {
		in := []int16{0, 1000, -1000, 32000, -32000}
		b, err := Encode(nil, f, in)
		if err != nil || len(b) != len(in) {
			t.Fatalf("%v %v", f, err)
		}
		out, err := Decode(nil, f, b)
		if err != nil || len(out) != len(in) {
			t.Fatalf("%v %v", f, err)
		}
	}
	if n := (Format{Encoding: PCMU, SampleRate: 8000}).Samples(20 * time.Millisecond); n != 160 {
		t.Errorf("samples %d", n)
	}
	r, err := NewResampler(8000, 16000)
	if err != nil {
		t.Fatal(err)
	}
	r.Process(nil, []int16{1, 2, 3, 4})
	if in, _ := r.Counts(); in != 4 {
		t.Errorf("counts in=%d", in)
	}
	if _, err := NewResampler(0, 16000); err == nil {
		t.Error("zero rate resampler")
	}
	if _, err := NewTwoPartyTimeline(0, 1); err == nil {
		t.Error("zero rate timeline")
	}
}

// A stalled leg must not grow the pending list without bound. Expired marks
// are kept for late echoes only up to MaxExpiredPending; past that the oldest
// are dropped. The accounting is unchanged: every expired chunk's Unconfirmed
// time is recorded once at expiry, and an unexpired mark is never dropped.
func TestPlayoutLedgerExpiredMarksAreBounded(t *testing.T) {
	l := newTestLedger(t)
	src := SourceRef{ItemID: "stalled"}
	const sent = MaxExpiredPending + 40
	for i := 0; i < sent; i++ {
		mustSend(t, l, src, 160, t0)
	}
	// Every chunk is overdue at the far time, so all of them expire.
	far := t0.Add(time.Hour)
	if err := l.Expire(far); !errors.Is(err, ErrPlayoutTimedOut) {
		t.Fatalf("Expire = %v, want ErrPlayoutTimedOut", err)
	}
	if got := l.Pending(); got != MaxExpiredPending {
		t.Fatalf("pending after stall = %d, want bounded to %d", got, MaxExpiredPending)
	}
	it, _ := l.Item(src)
	if want := time.Duration(sent) * 20 * time.Millisecond; it.Unconfirmed != want || it.Sent != want {
		t.Fatalf("unconfirmed %v sent %v, want both %v (each chunk counted once)", it.Unconfirmed, it.Sent, want)
	}
	// Expiring again must not recount what was already counted.
	if err := l.Expire(far.Add(time.Second)); err != nil {
		t.Fatalf("re-expire recounted: %v", err)
	}
	if it2, _ := l.Item(src); it2.Unconfirmed != it.Unconfirmed {
		t.Fatalf("unconfirmed changed on re-expire: %v -> %v", it.Unconfirmed, it2.Unconfirmed)
	}
}

// Unexpired marks are never dropped by the bound, so live audio stays fully
// accounted for and still counts toward PendingLive.
func TestPlayoutLedgerTrimKeepsUnexpiredMarks(t *testing.T) {
	l := newTestLedger(t)
	src := SourceRef{ItemID: "live"}
	for i := 0; i < MaxExpiredPending+4; i++ {
		mustSend(t, l, src, 160, t0)
	}
	if err := l.Expire(t0.Add(time.Hour)); !errors.Is(err, ErrPlayoutTimedOut) {
		t.Fatalf("Expire = %v, want ErrPlayoutTimedOut", err)
	}
	// A new live chunk, sent after the stall, is unexpired.
	live := mustSend(t, l, src, 160, t0.Add(time.Hour))
	if l.PendingLive() != 1 {
		t.Fatalf("PendingLive = %d, want the one live mark", l.PendingLive())
	}
	if err := l.Expire(t0.Add(time.Hour)); err != nil {
		t.Fatalf("live mark expired early: %v", err)
	}
	if _, err := l.Echo(live, t0.Add(time.Hour+time.Second)); err != nil {
		t.Fatalf("live mark was dropped: %v", err)
	}
}

// Expired chunks beyond MaxExpiredPending are trimmed from the pending list,
// but a genuine late echo for a trimmed chunk must still be accepted, as
// MarkEarly, without a second Unconfirmed count. Rejecting it would make the
// media layer close the stream with a policy violation for a call that is fine.
func TestPlayoutLedgerLateEchoOfTrimmedMarkIsAccepted(t *testing.T) {
	l := newTestLedger(t)
	src := SourceRef{ItemID: "trim"}
	const sent = MaxExpiredPending + 44
	marks := make([]string, 0, sent)
	for i := 0; i < sent; i++ {
		marks = append(marks, mustSend(t, l, src, 160, t0))
	}
	// Every chunk is overdue past the last one's end, so all expire and the
	// oldest 44 are trimmed.
	late := t0.Add(time.Duration(sent)*20*time.Millisecond + time.Second)
	if err := l.Expire(late); !errors.Is(err, ErrPlayoutTimedOut) {
		t.Fatalf("Expire = %v, want ErrPlayoutTimedOut", err)
	}
	if got := l.Pending(); got != MaxExpiredPending {
		t.Fatalf("pending = %d, want %d after trim", got, MaxExpiredPending)
	}
	before, _ := l.Item(src)
	for i, m := range marks {
		o, err := l.Echo(m, late)
		if err != nil {
			t.Fatalf("late echo %d (%s) was rejected: %v", i, m, err)
		}
		if o != MarkEarly {
			t.Fatalf("late echo %d classified %v, want early", i, o)
		}
	}
	after, _ := l.Item(src)
	if after != before {
		t.Fatalf("late echoes changed accounting: before %+v after %+v", before, after)
	}
	if want := time.Duration(sent) * 20 * time.Millisecond; after.Unconfirmed != want || after.Confirmed != 0 {
		t.Fatalf("unconfirmed %v confirmed %v, want %v and 0", after.Unconfirmed, after.Confirmed, want)
	}
	if l.Pending() != 0 {
		t.Fatalf("pending = %d after every echo, want 0", l.Pending())
	}
}

// A trimmed mark is accepted once. A second echo for it, and a mark that was
// never issued, are both unknown.
func TestPlayoutLedgerTrimmedEchoIsOnceAndUnissuedStaysUnknown(t *testing.T) {
	l := newTestLedger(t)
	src := SourceRef{ItemID: "once"}
	const sent = MaxExpiredPending + 2
	marks := make([]string, 0, sent)
	for i := 0; i < sent; i++ {
		marks = append(marks, mustSend(t, l, src, 160, t0))
	}
	late := t0.Add(time.Hour)
	if err := l.Expire(late); !errors.Is(err, ErrPlayoutTimedOut) {
		t.Fatalf("Expire = %v", err)
	}
	if _, err := l.Echo(marks[0], late); err != nil {
		t.Fatalf("first late echo of trimmed mark: %v", err)
	}
	if _, err := l.Echo(marks[0], late); !errors.Is(err, ErrUnknownMark) {
		t.Fatalf("second echo of trimmed mark = %v, want ErrUnknownMark", err)
	}
	for _, forged := range []string{"ch-0", "ch-99999", "ch-" + marks[len(marks)-1][3:] + "9", "bogus"} {
		if _, err := l.Echo(forged, late); !errors.Is(err, ErrUnknownMark) {
			t.Fatalf("never-issued mark %q = %v, want ErrUnknownMark", forged, err)
		}
	}
}

// A trimmed mark that is echoed out of order against a live predecessor still
// reports ErrMarkOutOfOrder, and does not disturb later classification.
func TestPlayoutLedgerTrimmedRangeSplitsCorrectly(t *testing.T) {
	ranges := []seqRange{{lo: 1, hi: 5}}
	l := &PlayoutLedger{trimmed: ranges}
	if !l.takeTrimmed(3) {
		t.Fatal("middle of range not taken")
	}
	want := []seqRange{{lo: 1, hi: 2}, {lo: 4, hi: 5}}
	if len(l.trimmed) != 2 || l.trimmed[0] != want[0] || l.trimmed[1] != want[1] {
		t.Fatalf("after split %v, want %v", l.trimmed, want)
	}
	if l.takeTrimmed(3) {
		t.Fatal("taken twice")
	}
	if !l.takeTrimmed(1) || !l.takeTrimmed(5) || !l.takeTrimmed(2) || !l.takeTrimmed(4) {
		t.Fatal("range ends not taken")
	}
	if len(l.trimmed) != 0 {
		t.Fatalf("ranges left %v", l.trimmed)
	}
}
