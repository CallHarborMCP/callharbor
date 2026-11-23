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
		{ProtocolRealtime, Requirements{Format: pcmu, TwoPartyTranscript: true}, true},
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
