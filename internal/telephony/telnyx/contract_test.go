package telnyx

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

const contractPath = "../../../api/upstream/telnyx/callcontrol.openapi.json"

// compileSchema compiles a component of the pinned official Telnyx OpenAPI
// extract. OpenAPI 3.1 component schemas are JSON Schema 2020-12.
func compileSchema(t *testing.T, pointer string) *jsonschema.Schema {
	t.Helper()
	f, err := os.Open(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("file:///telnyx.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("file:///telnyx.json#" + pointer)
	if err != nil {
		t.Fatalf("compile %s: %v", pointer, err)
	}
	return s
}

func instance(t *testing.T, b []byte) any {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestContractPinnedSource(t *testing.T) {
	b, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Source struct {
				Commit string `json:"commit"`
				SHA256 string `json:"sha256"`
			} `json:"x-callharbor-source"`
		} `json:"info"`
		Paths    map[string]json.RawMessage `json:"paths"`
		Webhooks map[string]json.RawMessage `json:"webhooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Info.Source.Commit != "bf2eb8ed5b816213987fecae8fe33329e98cdd84" || len(doc.Info.Source.SHA256) != 64 {
		t.Fatalf("extract is not pinned: %+v", doc.Info.Source)
	}
	for _, p := range []string{"/calls", "/calls/{call_control_id}/actions/hangup", "/calls/{call_control_id}/actions/streaming_start"} {
		if _, ok := doc.Paths[p]; !ok {
			t.Errorf("missing path %s", p)
		}
	}
	if !strings.Contains(string(doc.Webhooks["callHangup"]), "timestamp|payload") {
		t.Error("pinned spec no longer documents timestamp|payload signing")
	}
}

func validDial() DialParams {
	return DialParams{
		ConnectionID: "1494404757140276705",
		From:         "+13125550100",
		To:           "+420601234567",
		WebhookURL:   "https://callharbor.example/telnyx/webhook",
		StreamURL:    "wss://callharbor.example/telnyx/media/2f9c1d0e5b",
		Codec:        audio.Format{Encoding: audio.PCMU, SampleRate: 8000},
		CommandID:    "call-7f3a:dial",
		ClientState:  []byte(`{"call":"7f3a"}`),
		TimeoutSecs:  30,
		TimeLimit:    1800,
	}
}

func TestDialRequestMatchesOfficialSchema(t *testing.T) {
	s := compileSchema(t, "/components/schemas/CallRequest")
	for _, f := range []audio.Format{
		{Encoding: audio.PCMU, SampleRate: 8000},
		{Encoding: audio.PCMA, SampleRate: 8000},
		{Encoding: audio.L16, SampleRate: 16000, LittleEndian: true},
	} {
		p := validDial()
		p.Codec = f
		r, err := BuildDialRequest(p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(r)
		if err := s.Validate(instance(t, b)); err != nil {
			t.Errorf("%v: dial body violates Telnyx CallRequest: %v\n%s", f, err, b)
		}
	}
}

func TestOfficialSchemaRejectsCorruptedDial(t *testing.T) {
	s := compileSchema(t, "/components/schemas/CallRequest")
	r, err := BuildDialRequest(validDial())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(m map[string]any){
		"missing connection_id":  func(m map[string]any) { delete(m, "connection_id") },
		"bad bidirectional mode": func(m map[string]any) { m["stream_bidirectional_mode"] = "pcm" },
		"bad codec case":         func(m map[string]any) { m["stream_bidirectional_codec"] = "pcmu" },
		"bad rate":               func(m map[string]any) { m["stream_bidirectional_sampling_rate"] = 11025 },
		"bad track":              func(m map[string]any) { m["stream_track"] = "inbound" },
		"bad target legs":        func(m map[string]any) { m["stream_bidirectional_target_legs"] = "callee" },
		"time limit too high":    func(m map[string]any) { m["time_limit_secs"] = 14401 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			b, _ := json.Marshal(r)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			mut(m)
			b, _ = json.Marshal(m)
			if err := s.Validate(instance(t, b)); err == nil {
				t.Fatalf("official schema accepted corrupted body %s", b)
			}
		})
	}
}

func TestBuildDialRequestRejectsInvalid(t *testing.T) {
	cases := map[string]func(p *DialParams){
		"no connection":     func(p *DialParams) { p.ConnectionID = "" },
		"to not e164":       func(p *DialParams) { p.To = "00420601234567" },
		"from not e164":     func(p *DialParams) { p.From = "+0123" },
		"http webhook":      func(p *DialParams) { p.WebhookURL = "http://callharbor.example/hook" },
		"ws stream":         func(p *DialParams) { p.StreamURL = "ws://callharbor.example/media" },
		"stream userinfo":   func(p *DialParams) { p.StreamURL = "wss://u:p@callharbor.example/media" },
		"unsupported codec": func(p *DialParams) { p.Codec = audio.Format{Encoding: "OPUS", SampleRate: 16000} },
		"l16 big endian":    func(p *DialParams) { p.Codec = audio.Format{Encoding: audio.L16, SampleRate: 16000} },
		"pcmu 16k":          func(p *DialParams) { p.Codec = audio.Format{Encoding: audio.PCMU, SampleRate: 16000} },
		"no command id":     func(p *DialParams) { p.CommandID = "" },
		"time limit":        func(p *DialParams) { p.TimeLimit = 10 },
		"auth token size":   func(p *DialParams) { p.StreamAuthToken = strings.Repeat("a", 4001) },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			p := validDial()
			mut(&p)
			if _, err := BuildDialRequest(p); err == nil {
				t.Fatal("accepted invalid dial parameters")
			}
		})
	}
}

func TestDialRequestFields(t *testing.T) {
	r, err := BuildDialRequest(validDial())
	if err != nil {
		t.Fatal(err)
	}
	if r.StreamBidirectionalMode != "rtp" || r.StreamTrack != "inbound_track" || r.StreamBidirectionalTargetLegs != "self" {
		t.Fatalf("unexpected stream configuration %+v", r)
	}
	if r.StreamCodec != r.StreamBidirectionalCodec || r.StreamBidirectionalSamplingRate != 8000 {
		t.Fatalf("inbound and bidirectional codecs differ: %+v", r)
	}
	if r.ClientState != "eyJjYWxsIjoiN2YzYSJ9" {
		t.Fatalf("client_state not base64: %q", r.ClientState)
	}
}

func TestOfficialWebhookExamplesValidateAndParse(t *testing.T) {
	cases := map[string]struct {
		schema string
		typ    string
		check  func(t *testing.T, ev WebhookEvent)
	}{
		"callInitiated": {"CallInitiatedEvent", EventCallInitiated, nil},
		"callAnswered":  {"CallAnsweredEvent", EventCallAnswered, nil},
		"callHangup": {"CallHangupEvent", EventCallHangup, func(t *testing.T, ev WebhookEvent) {
			if ev.HangupCause != "call_rejected" || ev.HangupSource != "caller" || ev.SIPCause != "603" {
				t.Errorf("hangup fields %+v", ev)
			}
		}},
		"callStreamingStarted": {"CallStreamingStartedEvent", EventStreamingStarted, nil},
		"callStreamingStopped": {"CallStreamingStoppedEvent", EventStreamingStopped, nil},
		"callStreamingFailed": {"CallStreamingFailedEvent", EventStreamingFailed, func(t *testing.T, ev WebhookEvent) {
			if ev.FailureReason != "connection_failed" || ev.StreamID == "" {
				t.Errorf("streaming failed fields %+v", ev)
			}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "webhooks", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := compileSchema(t, "/components/schemas/"+tc.schema).Validate(instance(t, b)); err != nil {
				t.Fatalf("official example does not match official schema: %v", err)
			}
			ev, err := ParseWebhook(b)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Type != tc.typ || !ev.Known || ev.ID == "" || ev.OccurredAt.IsZero() {
				t.Fatalf("parsed %+v", ev)
			}
			if ev.CallControlID == "" {
				t.Fatal("missing call_control_id")
			}
			if tc.check != nil {
				tc.check(t, ev)
			}
		})
	}
}

// The official call.machine.detection.ended example still matches the pinned
// schema (the upstream reference is unchanged), but CallHarbor no longer
// consumes it: it parses as unknown and never drives state.
func TestMachineDetectionWebhookIsUnknownAfterTelephonyOnly(t *testing.T) {
	b := loadWebhook(t, "callMachineDetectionEnded")
	if err := compileSchema(t, "/components/schemas/CallMachineDetectionEndedEvent").Validate(instance(t, b)); err != nil {
		t.Fatalf("official example does not match official schema: %v", err)
	}
	ev, err := ParseWebhook(b)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Known || ev.Type != "call.machine.detection.ended" {
		t.Fatalf("parsed %+v, want unknown call.machine.detection.ended", ev)
	}
	if _, ok := ev.Signal(); ok {
		t.Fatal("machine detection webhook mapped to a state signal")
	}
}

func TestParseWebhookRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"not json":        `{"data":`,
		"no data":         `{"meta":{}}`,
		"wrong record":    `{"data":{"record_type":"call","event_type":"call.hangup","id":"x","payload":{"call_control_id":"c"}}}`,
		"no id":           `{"data":{"record_type":"event","event_type":"call.hangup","payload":{"call_control_id":"c"}}}`,
		"no call id":      `{"data":{"record_type":"event","event_type":"call.hangup","id":"x","payload":{}}}`,
		"bad time":        `{"data":{"record_type":"event","event_type":"call.hangup","id":"x","occurred_at":"yesterday","payload":{"call_control_id":"c"}}}`,
		"trailing":        `{"data":{"record_type":"event","event_type":"call.hangup","id":"x","payload":{"call_control_id":"c"}}} {}`,
		"payload not obj": `{"data":{"record_type":"event","event_type":"call.hangup","id":"x","payload":"c"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseWebhook([]byte(body)); err == nil {
				t.Fatal("accepted malformed webhook")
			}
		})
	}
	if _, err := ParseWebhook(bytes.Repeat([]byte(" "), MaxWebhookBody+1)); err == nil {
		t.Fatal("accepted oversized body")
	}
	ev, err := ParseWebhook([]byte(`{"data":{"record_type":"event","event_type":"call.speak.started","id":"x","payload":{}}}`))
	if err != nil || ev.Known {
		t.Fatalf("unknown event types must be preserved, not rejected: %+v %v", ev, err)
	}
}
