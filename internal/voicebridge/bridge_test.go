package voicebridge

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

var (
	g711 = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}
	l16k = audio.Format{Encoding: audio.L16, SampleRate: 16000, LittleEndian: true}
	l24k = audio.Format{Encoding: audio.L16, SampleRate: 24000, LittleEndian: true}
)

func newT(t *testing.T, leg, provider audio.Format) *Bridge {
	t.Helper()
	b, err := New(leg, provider, 5*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNewRejectsInvalidFormats(t *testing.T) {
	if _, err := New(audio.Format{Encoding: "OPUS", SampleRate: 48000}, g711, time.Millisecond, time.Second); !errors.Is(err, ErrFormat) {
		t.Fatalf("leg: %v", err)
	}
	if _, err := New(g711, audio.Format{Encoding: "OPUS", SampleRate: 48000}, time.Millisecond, time.Second); !errors.Is(err, ErrFormat) {
		t.Fatalf("provider: %v", err)
	}
}

func TestPassthroughG711IsByteExact(t *testing.T) {
	b := newT(t, g711, g711)
	in := tone(440, 20*time.Millisecond)
	out, err := b.CallerAudio(in)
	if err != nil || string(out) != string(in) {
		t.Fatalf("caller passthrough changed bytes: %v", err)
	}
	p, _, err := b.AgentAudio(audio.SourceRef{ItemID: "a"}, in, time.Unix(0, 0))
	if err != nil || string(p) != string(in) {
		t.Fatalf("agent passthrough changed bytes: %v", err)
	}
}

func TestPCMAPassthroughIsByteExact(t *testing.T) {
	alaw := audio.Format{Encoding: audio.PCMA, SampleRate: 8000}
	b := newT(t, alaw, alaw)
	in := alawTone(440, 20*time.Millisecond)
	out, err := b.CallerAudio(in)
	if err != nil || string(out) != string(in) {
		t.Fatalf("caller A-law passthrough changed bytes: %v", err)
	}
	p, _, err := b.AgentAudio(audio.SourceRef{ItemID: "a"}, in, time.Unix(0, 0))
	if err != nil || string(p) != string(in) {
		t.Fatalf("agent A-law passthrough changed bytes: %v", err)
	}
}

func TestPCMAToL16ConvertsCodecAndRate(t *testing.T) {
	alaw := audio.Format{Encoding: audio.PCMA, SampleRate: 8000}
	b := newT(t, alaw, l24k)
	in := alawTone(440, 40*time.Millisecond)
	up, err := b.CallerAudio(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := 40 * 24 * 2; len(up) < want-2*2 || len(up) > want {
		t.Fatalf("caller L16 24k bytes %d, want about %d", len(up), want)
	}
	p, _, err := b.AgentAudio(audio.SourceRef{ItemID: "a"}, up, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != len(in) {
		t.Fatalf("agent A-law bytes %d want %d", len(p), len(in))
	}
	var maxErr int
	for i := range in {
		d := int(audio.AlawToLinear(in[i])) - int(audio.AlawToLinear(p[i]))
		if d < 0 {
			d = -d
		}
		if d > maxErr {
			maxErr = d
		}
	}
	if maxErr > 1200 {
		t.Fatalf("A-law round trip error %d exceeds G.711 plus interpolation bound", maxErr)
	}
}

func TestCodecAndRateConversionRoundTrip(t *testing.T) {
	b := newT(t, g711, l24k)
	in := tone(440, 40*time.Millisecond)
	up, err := b.CallerAudio(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := 40 * 24 * 2; len(up) < want-2*2 || len(up) > want {
		t.Fatalf("caller L16 24k bytes %d, want about %d", len(up), want)
	}
	p, mark, err := b.AgentAudio(audio.SourceRef{ItemID: "a"}, up, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != len(in) || mark == "" {
		t.Fatalf("agent bytes %d want %d mark %q", len(p), len(in), mark)
	}
	var maxErr int
	for i := range in {
		d := int(audio.UlawToLinear(in[i])) - int(audio.UlawToLinear(p[i]))
		if d < 0 {
			d = -d
		}
		if d > maxErr {
			maxErr = d
		}
	}
	if maxErr > 1200 {
		t.Fatalf("round trip error %d exceeds G.711 plus interpolation bound", maxErr)
	}
}

func TestOddLengthL16Rejected(t *testing.T) {
	b := newT(t, g711, l16k)
	if _, _, err := b.AgentAudio(audio.SourceRef{ItemID: "a"}, []byte{1, 2, 3}, time.Unix(0, 0)); err == nil {
		t.Fatal("odd L16 accepted")
	}
	if _, err := b.CallerAudio(nil); err == nil {
		t.Fatal("empty caller payload accepted")
	}
}

func TestBargeInDoesNotCountUnechoedAudio(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := newT(t, g711, g711)
	src := audio.SourceRef{ItemID: "agent-1"}
	for i := 0; i < 50; i++ {
		if _, _, err := b.AgentAudio(src, tone(660, 20*time.Millisecond), t0); err != nil {
			t.Fatal(err)
		}
	}
	b.Interrupt(t0.Add(330 * time.Millisecond))
	it, ok := b.Item(src)
	if !ok || it.Confirmed != 0 || it.Sent != time.Second {
		t.Fatalf("before any echo: %+v", it)
	}
	if err := b.Expire(t0.Add(time.Hour)); !errors.Is(err, audio.ErrPlayoutTimedOut) {
		t.Fatalf("pending marks must time out, got %v", err)
	}
}

func tone(hz float64, d time.Duration) []byte {
	n := int(d / (time.Second / 8000))
	out := make([]byte, n)
	for i := range out {
		out[i] = audio.LinearToUlaw(int16(8000 * math.Sin(2*math.Pi*hz*float64(i)/8000)))
	}
	return out
}

func alawTone(hz float64, d time.Duration) []byte {
	n := int(d / (time.Second / 8000))
	out := make([]byte, n)
	for i := range out {
		out[i] = audio.LinearToAlaw(int16(8000 * math.Sin(2*math.Pi*hz*float64(i)/8000)))
	}
	return out
}
