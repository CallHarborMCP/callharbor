package audio

import (
	"math"
	"testing"
	"time"
)

const vadRate = 8000

// tone returns d of a sine at amp (int16 peak) at vadRate.
func tone(d time.Duration, amp float64) []int16 {
	n := int(int64(d) * vadRate / int64(time.Second))
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*440*float64(i)/vadRate))
	}
	return out
}

// noiseFloor returns d of a deterministic low-level pattern below threshold.
func noiseFloor(d time.Duration) []int16 {
	n := int(int64(d) * vadRate / int64(time.Second))
	out := make([]int16, n)
	for i := range out {
		out[i] = int16((i*37)%41 - 20)
	}
	return out
}

func feed(t *testing.T, v *VAD, s []int16, chunk int) []SpeechTransition {
	t.Helper()
	var all []SpeechTransition
	for len(s) > 0 {
		n := chunk
		if n > len(s) {
			n = len(s)
		}
		all = append(all, v.Process(s[:n])...)
		s = s[n:]
	}
	return all
}

func TestVADSilenceAndNoiseFloorNeverStart(t *testing.T) {
	v, err := NewVAD(vadRate)
	if err != nil {
		t.Fatal(err)
	}
	got := feed(t, v, noiseFloor(2*time.Second), 160)
	if len(got) != 0 || v.Speaking() {
		t.Fatalf("noise floor produced transitions %v", got)
	}
}

func TestVADToneStartsOnceAndEndsAfterHangover(t *testing.T) {
	v, err := NewVAD(vadRate)
	if err != nil {
		t.Fatal(err)
	}
	var got []SpeechTransition
	got = append(got, feed(t, v, noiseFloor(200*time.Millisecond), 160)...)
	got = append(got, feed(t, v, tone(500*time.Millisecond, 4000), 160)...)
	got = append(got, feed(t, v, noiseFloor(time.Second), 160)...)
	if len(got) != 2 || got[0] != SpeechStart || got[1] != SpeechEnd {
		t.Fatalf("transitions %v, want [start end]", got)
	}
	if v.Speaking() {
		t.Fatal("still speaking after hangover")
	}
}

func TestVADOnsetRequiresSustainedSpeech(t *testing.T) {
	v, err := NewVAD(vadRate)
	if err != nil {
		t.Fatal(err)
	}
	// A 40 ms click is shorter than the 60 ms onset window.
	got := feed(t, v, tone(40*time.Millisecond, 4000), 160)
	got = append(got, feed(t, v, noiseFloor(time.Second), 160)...)
	for _, tr := range got {
		if tr == SpeechStart {
			t.Fatal("a click shorter than the onset window started speech")
		}
	}
}

func TestVADHangoverBridgesShortPauses(t *testing.T) {
	v, err := NewVAD(vadRate)
	if err != nil {
		t.Fatal(err)
	}
	var seq []SpeechTransition
	seq = append(seq, feed(t, v, tone(300*time.Millisecond, 4000), 160)...)
	// 200 ms pause is shorter than the 400 ms hangover: no End.
	seq = append(seq, feed(t, v, noiseFloor(200*time.Millisecond), 160)...)
	seq = append(seq, feed(t, v, tone(300*time.Millisecond, 4000), 160)...)
	if want := []SpeechTransition{SpeechStart}; len(seq) != 1 || seq[0] != want[0] {
		t.Fatalf("transitions %v, want exactly one start across the pause", seq)
	}
	if !v.Speaking() {
		t.Fatal("pause shorter than hangover ended speech")
	}
}

func TestVADChunkSizeIndependent(t *testing.T) {
	var ref []SpeechTransition
	signal := append(append(noiseFloor(100*time.Millisecond), tone(400*time.Millisecond, 3000)...), noiseFloor(900*time.Millisecond)...)
	for _, chunk := range []int{1, 7, 20, 160, 161, 320, 1000} {
		v, err := NewVAD(vadRate)
		if err != nil {
			t.Fatal(err)
		}
		got := feed(t, v, signal, chunk)
		if ref == nil {
			ref = got
			if len(ref) != 2 || ref[0] != SpeechStart || ref[1] != SpeechEnd {
				t.Fatalf("reference transitions %v, want [start end]", ref)
			}
			continue
		}
		if len(got) != len(ref) {
			t.Fatalf("chunk %d: transitions %v, want %v", chunk, got, ref)
		}
		for i := range got {
			if got[i] != ref[i] {
				t.Fatalf("chunk %d: transitions %v, want %v", chunk, got, ref)
			}
		}
	}
}

func TestVADRejectsUnsupportedRate(t *testing.T) {
	if _, err := NewVAD(44100); err == nil {
		t.Fatal("unsupported rate accepted")
	}
}
