package app

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx/telnyxfake"
)

// recFrameSamples is one 20 ms PCMU frame at 8 kHz.
const recFrameSamples = 160

// recLoudPCMU is one 20 ms frame of a 440 Hz tone, base64 PCMU. Its level is
// well above the local speech detector's threshold.
func recLoudPCMU(t *testing.T) string {
	t.Helper()
	pcm := make([]int16, recFrameSamples)
	for i := range pcm {
		pcm[i] = int16(12000 * math.Sin(2*math.Pi*440*float64(i)/8000))
	}
	return recEncodePCMU(t, pcm)
}

// recQuietPCMU is one 20 ms frame of digital silence, base64 PCMU.
func recQuietPCMU(t *testing.T) string {
	t.Helper()
	return recEncodePCMU(t, make([]int16, recFrameSamples))
}

func recEncodePCMU(t *testing.T, pcm []int16) string {
	t.Helper()
	b, err := audio.Encode(nil, audio.Format{Encoding: audio.PCMU, SampleRate: 8000}, pcm)
	if err != nil {
		t.Fatalf("encode PCMU: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// recSendMediaAt sends one inbound Telnyx media frame with an explicit RTP
// timestamp. Consecutive frames advance the timestamp by recFrameSamples; a
// larger step leaves a gap the recorder must fill with silence.
func recSendMediaAt(t *testing.T, s *telnyxfake.Stream, seq int, ts uint32, payload string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"event": "media", "sequence_number": seqString(seq), "stream_id": "s1",
		"media": map[string]any{
			"track": "inbound", "chunk": seqString(seq),
			"timestamp": seqString(int(ts)), "payload": payload,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), b); err != nil {
		t.Fatalf("send media seq %d ts %d: %v", seq, ts, err)
	}
}

func seqString(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// recWAV is a parsed two-channel 16-bit recording.
type recWAV struct {
	rate  int
	ch0   []int16 // remote party
	ch1   []int16 // agent
	bytes int     // total file length
}

// recParseWAV checks the canonical 44-byte header and splits the stereo PCM.
func recParseWAV(t *testing.T, b []byte) recWAV {
	t.Helper()
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" ||
		string(b[12:16]) != "fmt " || string(b[36:40]) != "data" {
		t.Fatalf("not a canonical WAV header: %q", b[:min(len(b), 44)])
	}
	channels := int(binary.LittleEndian.Uint16(b[22:24]))
	bits := int(binary.LittleEndian.Uint16(b[34:36]))
	if channels != 2 || bits != 16 {
		t.Fatalf("format = %d channels %d-bit, want stereo 16-bit", channels, bits)
	}
	if n := int(binary.LittleEndian.Uint32(b[40:44])); n != len(b)-44 {
		t.Fatalf("data chunk says %d bytes, file has %d", n, len(b)-44)
	}
	w := recWAV{rate: int(binary.LittleEndian.Uint32(b[24:28])), bytes: len(b)}
	pcm := b[44:]
	for i := 0; i+4 <= len(pcm); i += 4 {
		w.ch0 = append(w.ch0, int16(binary.LittleEndian.Uint16(pcm[i:])))
		w.ch1 = append(w.ch1, int16(binary.LittleEndian.Uint16(pcm[i+2:])))
	}
	return w
}

// recRMS is the root-mean-square level of s over frames [from, to).
func recRMS(s []int16, from, to int) float64 {
	from = max(from, 0)
	to = min(to, len(s))
	if to <= from {
		return 0
	}
	var sum float64
	for _, v := range s[from:to] {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(to-from))
}
