package realtime

import (
	"errors"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

func TestCapabilityMatrix(t *testing.T) {
	pcmu := audio.Format{Encoding: audio.PCMU, SampleRate: 8000}
	pcm16k := audio.Format{Encoding: audio.L16, SampleRate: 16000, LittleEndian: true}
	cases := []struct {
		p    Protocol
		r    Requirements
		fail bool
	}{
		{ProtocolRealtime, Requirements{Format: pcmu}, false},
		{ProtocolRealtime, Requirements{Format: pcm16k}, true},
		{ProtocolRealtime, Requirements{Format: pcmu, TwoPartyTranscript: true}, false},
		{ProtocolRealtime, Requirements{Format: pcm16k, TwoPartyTranscript: true}, true},
		{ProtocolLive, Requirements{Format: pcmu, TwoPartyTranscript: true}, false},
		{ProtocolLive, Requirements{Format: pcm16k, TwoPartyTranscript: true}, false},
		{ProtocolLive, Requirements{Format: audio.Format{Encoding: audio.L16, SampleRate: 16000}}, true},
	}
	for _, tc := range cases {
		c, err := For(tc.p)
		if err != nil {
			t.Fatal(err)
		}
		err = c.Check(tc.r)
		if tc.fail != (err != nil) || (err != nil && !errors.Is(err, ErrCapability)) {
			t.Errorf("%s %+v: err=%v", tc.p, tc.r, err)
		}
	}
	if _, err := For("gpt-live-1"); err == nil {
		t.Fatal("protocol must not be inferred from a model name")
	}
	if RealtimeCapabilities.OutputPlayoutEvents != Unavailable || LiveCapabilities.OutputPlayoutEvents != Unavailable {
		t.Fatal("neither WebSocket protocol reports playout; telephony marks are the only evidence")
	}
	if LiveCapabilities.ServerTruncate || LiveCapabilities.ModelInURL || !RealtimeCapabilities.ModelInURL {
		t.Fatal("protocol differences regressed")
	}
	if RealtimeCapabilities.SpeechStartEvents != Native || LiveCapabilities.SpeechStartEvents != Unavailable {
		t.Fatal("speech-start events: realtime_ws is native, live_ws has none documented")
	}
	// Caller-transcript contract matrix: out-of-band passes, billed and
	// unavailable fail closed, and V1 reports the availability honestly.
	for _, c := range []struct {
		name string
		ca   Availability
		ok   bool
		v1   Availability
	}{
		{"native", Native, true, Native},
		{"out_of_band", OutOfBand, true, OutOfBand},
		{"opt_in_billed", OptInBilled, false, Unavailable},
		{"unavailable", Unavailable, false, Unavailable},
	} {
		caps := RealtimeCapabilities
		caps.CallerTranscript = c.ca
		err := caps.Check(Requirements{Format: pcmu, TwoPartyTranscript: true})
		if c.ok != (err == nil) || (err != nil && !errors.Is(err, ErrCapability)) {
			t.Errorf("caller transcript %s: err=%v", c.name, err)
		}
		if got := caps.V1CallerTranscript(); got != c.v1 {
			t.Errorf("caller transcript %s: V1=%q want %q", c.name, got, c.v1)
		}
	}
	if RealtimeCapabilities.CallerTranscript != OutOfBand || LiveCapabilities.CallerTranscript != Native {
		t.Fatal("realtime_ws caller transcript must be out-of-band; live_ws native")
	}
	if RealtimeCapabilities.DTMFInput != Unavailable || LiveCapabilities.DTMFInput != Unavailable {
		t.Fatal("neither primary WebSocket accepts DTMF input; digits must never reach the model")
	}
}

func TestValidateEndpoint(t *testing.T) {
	for _, ok := range []string{"wss://api.openai.com/v1/realtime", "ws://127.0.0.1:8080/v1/live/sessions", "ws://localhost/v1/realtime", "ws://[::1]:9/x"} {
		if _, err := ValidateEndpoint(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "https://api.openai.com/v1/realtime", "ws://10.0.0.1/v1/realtime", "ws://example.com/x", "wss://k@api.openai.com/x", "wss://api.openai.com/x#f", "/v1/realtime"} {
		if _, err := ValidateEndpoint(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
