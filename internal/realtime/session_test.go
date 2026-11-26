package realtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

func TestValidateHeaders(t *testing.T) {
	h, err := ValidateHeaders(map[string]string{"x-tenant": "acme", "openai-organization": "org_1"})
	if err != nil {
		t.Fatal(err)
	}
	if h.Get("X-Tenant") != "acme" || h.Get("Openai-Organization") != "org_1" {
		t.Fatalf("headers %v", h)
	}
	if got, err := ValidateHeaders(nil); err != nil || len(got) != 0 {
		t.Fatalf("nil headers: %v %v", got, err)
	}
	bad := map[string]map[string]string{
		"reserved auth":      {"Authorization": "Bearer other"},
		"reserved lowercase": {"authorization": "x"},
		"upgrade":            {"Upgrade": "h2c"},
		"hop-by-hop":         {"Transfer-Encoding": "chunked"},
		"ws key":             {"Sec-WebSocket-Key": "x"},
		"space in name":      {"X Bad": "v"},
		"empty name":         {"": "v"},
		"newline value":      {"X-A": "a\r\nInjected: 1"},
		"nul value":          {"X-A": "a\x00b"},
		"oversized value":    {"X-A": strings.Repeat("v", MaxHeaderValue+1)},
	}
	for name, in := range bad {
		if _, err := ValidateHeaders(in); err == nil {
			t.Errorf("%s: accepted %v", name, in)
		}
	}
	many := map[string]string{}
	for i := 0; i <= MaxHeaders; i++ {
		many[strings.Repeat("h", i+1)] = "v"
	}
	if _, err := ValidateHeaders(many); err == nil {
		t.Fatal("accepted too many headers")
	}
}

func TestTransportClassification(t *testing.T) {
	if err := Transport(context.Canceled); !errors.Is(err, context.Canceled) || errors.Is(err, ErrDisconnected) {
		t.Fatalf("cancellation must not look like a provider disconnect: %v", err)
	}
	if err := Transport(context.DeadlineExceeded); errors.Is(err, ErrDisconnected) {
		t.Fatalf("deadline must not look like a provider disconnect: %v", err)
	}
	if err := Transport(errors.New("read tcp: reset")); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("socket failure must be a disconnect: %v", err)
	}
	proto := errors.Join(ErrProtocol, errors.New("bad"))
	if err := Transport(proto); errors.Is(err, ErrDisconnected) {
		t.Fatalf("protocol violation must not be reported as a disconnect: %v", err)
	}
	if Transport(nil) != nil {
		t.Fatal("nil must stay nil")
	}
}

func TestDialFailsClosedBeforeAnyAdapter(t *testing.T) {
	called := false
	d := Dialers{ProtocolRealtime: func(context.Context, Profile) (Session, error) {
		called = true
		return nil, nil
	}}
	pcmu := audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

	if _, err := d.Dial(context.Background(), Profile{Protocol: "gpt-live-1"}, Requirements{Format: pcmu}); err == nil {
		t.Fatal("unknown protocol accepted")
	}
	if _, err := d.Dial(context.Background(), Profile{Protocol: ProtocolLive}, Requirements{Format: pcmu}); !errors.Is(err, ErrNoAdapter) {
		t.Fatalf("live without adapter must not fall back to realtime: %v", err)
	}
	if _, err := d.Dial(context.Background(), Profile{Protocol: ProtocolRealtime}, Requirements{Format: audio.Format{Encoding: audio.L16, SampleRate: 16000, LittleEndian: true}}); !errors.Is(err, ErrCapability) {
		t.Fatalf("unsupported format: %v", err)
	}
	if called {
		t.Fatal("an adapter ran for a rejected profile")
	}
	if _, err := d.Dial(context.Background(), Profile{Protocol: ProtocolRealtime}, Requirements{Format: pcmu}); err != nil || !called {
		t.Fatalf("supported profile not dialed: %v", err)
	}
	called = false
	if _, err := d.Dial(context.Background(), Profile{Protocol: ProtocolRealtime}, Requirements{Format: pcmu, TwoPartyTranscript: true}); err != nil || !called {
		t.Fatalf("realtime_ws out-of-band two-party transcript must be dialed: %v", err)
	}
}
