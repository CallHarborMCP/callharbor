package audio

import (
	"errors"
	"math"
	"testing"
	"time"
)

// ITU-T G.711 reference points.
func TestG711KnownValues(t *testing.T) {
	ulaw := map[byte]int16{0xFF: 0, 0x7F: 0, 0x80: 32124, 0x00: -32124, 0xF0: 120, 0x70: -120}
	for b, want := range ulaw {
		if got := UlawToLinear(b); got != want {
			t.Errorf("ulaw %#x -> %d, want %d", b, got, want)
		}
	}
	alaw := map[byte]int16{0xD5: 8, 0x55: -8, 0xAA: 32256, 0x2A: -32256}
	for b, want := range alaw {
		if got := AlawToLinear(b); got != want {
			t.Errorf("alaw %#x -> %d, want %d", b, got, want)
		}
	}
	if LinearToUlaw(0) != 0xFF || LinearToAlaw(0) != 0xD5 {
		t.Error("silence encoding")
	}
}

func TestG711RoundTripAllCodes(t *testing.T) {
	for i := 0; i < 256; i++ {
		b := byte(i)
		if b == 0x7F { // μ-law negative zero decodes to 0 and re-encodes as 0xFF
			continue
		}
		if got := LinearToUlaw(UlawToLinear(b)); got != b {
			t.Errorf("ulaw %#x round trip -> %#x", b, got)
		}
		if got := LinearToAlaw(AlawToLinear(b)); got != b {
			t.Errorf("alaw %#x round trip -> %#x", b, got)
		}
	}
}

func TestG711QuantizationError(t *testing.T) {
	for v := -32768; v <= 32767; v += 7 {
		s := int16(v)
		for name, rt := range map[string]int16{"ulaw": UlawToLinear(LinearToUlaw(s)), "alaw": AlawToLinear(LinearToAlaw(s))} {
			e := math.Abs(float64(rt) - float64(s))
			limit := math.Max(16, math.Abs(float64(s))/16)
			if s > 32635 || s < -32635 {
				limit = 1100
			}
			if e > limit {
				t.Fatalf("%s: %d -> %d error %v > %v", name, s, rt, e, limit)
			}
		}
	}
}

func TestEncodeDecodeL16Endianness(t *testing.T) {
	le := Format{Encoding: L16, SampleRate: 16000, LittleEndian: true}
	be := Format{Encoding: L16, SampleRate: 16000}
	b, _ := Encode(nil, le, []int16{0x0102, -2})
	if b[0] != 0x02 || b[1] != 0x01 || b[2] != 0xFE || b[3] != 0xFF {
		t.Fatalf("LE bytes %x", b)
	}
	s, _ := Decode(nil, be, b)
	if s[0] != 0x0201 {
		t.Fatalf("BE decode of LE bytes must differ: %#x", s[0])
	}
	if _, err := Decode(nil, le, []byte{1}); err == nil {
		t.Fatal("odd L16 accepted")
	}
}

func TestFormatValidationAndDuration(t *testing.T) {
	for _, f := range []Format{{Encoding: PCMU, SampleRate: 16000}, {Encoding: "OPUS", SampleRate: 16000}, {Encoding: L16, SampleRate: 22050}} {
		if f.Validate() == nil {
			t.Errorf("accepted %v", f)
		}
	}
	d, err := Format{Encoding: PCMU, SampleRate: 8000}.Duration(160)
	if err != nil || d != 20*time.Millisecond {
		t.Fatalf("%v %v", d, err)
	}
	d, err = Format{Encoding: L16, SampleRate: 24000, LittleEndian: true}.Duration(960)
	if err != nil || d != 20*time.Millisecond {
		t.Fatalf("%v %v", d, err)
	}
	if _, err := (Format{Encoding: L16, SampleRate: 24000}).Duration(961); err == nil {
		t.Fatal("partial sample accepted")
	}
}

func sine(rate int, hz float64, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(10000 * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
	}
	return out
}

// Chunked resampling must produce exactly the same samples and count as
// one-shot processing, i.e. no clock drift accumulates over a call.
func TestResamplerNoDriftAcrossChunking(t *testing.T) {
	for _, rates := range [][2]int{{8000, 24000}, {24000, 8000}, {8000, 16000}, {16000, 24000}, {24000, 16000}} {
		in := sine(rates[0], 300, rates[0]*60) // 60 s
		one, _ := NewResampler(rates[0], rates[1])
		want := one.Flush(one.Process(nil, in))
		r, _ := NewResampler(rates[0], rates[1])
		var got []int16
		for i := 0; i < len(in); {
			n := 1 + (i*7919)%331 // irregular packet sizes
			if i+n > len(in) {
				n = len(in) - i
			}
			got = r.Process(got, in[i:i+n])
			i += n
		}
		got = r.Flush(got)
		if len(got) != rates[1]*60 || len(got) != len(want) {
			t.Fatalf("%v: got %d samples, want %d", rates, len(got), rates[1]*60)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%v: sample %d differs %d != %d", rates, i, got[i], want[i])
			}
		}
	}
}

func TestResamplerPreservesTone(t *testing.T) {
	in := sine(8000, 400, 8000)
	r, _ := NewResampler(8000, 24000)
	out := r.Flush(r.Process(nil, in))
	ref := sine(24000, 400, 24000)
	var errSum, sigSum float64
	for i := range ref {
		d := float64(out[i]) - float64(ref[i])
		errSum += d * d
		sigSum += float64(ref[i]) * float64(ref[i])
	}
	if snr := 10 * math.Log10(sigSum/errSum); snr < 25 {
		t.Fatalf("SNR %.1f dB too low", snr)
	}
}

func TestRTPClockWrapLossAndReorder(t *testing.T) {
	var c RTPClock
	start := uint32(0xFFFFFF00)
	pos, gap, err := c.Position(start, 0)
	if pos != 0 || gap != 0 || err != nil {
		t.Fatal(pos, gap, err)
	}
	end := int64(160)
	pos, gap, err = c.Position(start+160, end) // wraps past 2^32
	if pos != 160 || gap != 0 || err != nil {
		t.Fatal(pos, gap, err)
	}
	end = pos + 160
	pos, gap, err = c.Position(start+640, end) // two frames lost
	if pos != 640 || gap != 320 || err != nil {
		t.Fatal(pos, gap, err)
	}
	end = pos + 160
	if _, _, err := c.Position(start+480, end); !errors.Is(err, ErrStaleFrame) {
		t.Fatalf("late frame: %v", err)
	}
	if _, _, err := c.Position(start+640, end); !errors.Is(err, ErrStaleFrame) {
		t.Fatalf("duplicate frame: %v", err)
	}
	if _, _, err := c.Position(start+800+8000*61, end); err == nil {
		t.Fatal("implausible jump accepted")
	}
}

func TestTwoPartyTimelineAlignment(t *testing.T) {
	tl, _ := NewTwoPartyTimeline(8000, 8000)
	if err := tl.Write(0, 0, []int16{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	if err := tl.Write(1, 2, []int16{9, 9}); err != nil {
		t.Fatal(err)
	}
	if err := tl.Write(0, 6, []int16{5}); err != nil {
		t.Fatal(err)
	}
	got := tl.Flush(nil, 8)
	want := []int16{1, 0, 2, 0, 3, 9, 4, 9, 0, 0, 0, 0, 5, 0, 0, 0}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("interleave %v want %v", got, want)
		}
	}
	if g := tl.GapSamples(); g[0] != 3 || g[1] != 6 {
		t.Fatalf("gaps %v", g)
	}
	if err := tl.Write(0, 7, []int16{1}); !errors.Is(err, ErrStaleFrame) {
		t.Fatalf("overlap into flushed region: %v", err)
	}
	if err := tl.Write(1, 8, make([]int16, 8001)); !errors.Is(err, ErrTimelineOverflow) {
		t.Fatalf("horizon: %v", err)
	}
	if err := tl.Write(2, 8, nil); err == nil {
		t.Fatal("bad channel accepted")
	}
}

func TestPlayoutLedgerValidation(t *testing.T) {
	if _, err := NewPlayoutLedger(Format{Encoding: "OPUS", SampleRate: 48000}, 0, time.Second); err == nil {
		t.Fatal("bad format")
	}
	if _, err := NewPlayoutLedger(Format{Encoding: PCMU, SampleRate: 8000}, 0, 0); err == nil {
		t.Fatal("zero grace")
	}
	l, _ := NewPlayoutLedger(Format{Encoding: L16, SampleRate: 24000, LittleEndian: true}, 0, time.Second)
	if _, err := l.Send(SourceRef{ItemID: "a"}, 3, time.Now()); err == nil {
		t.Fatal("partial sample chunk accepted")
	}
}
