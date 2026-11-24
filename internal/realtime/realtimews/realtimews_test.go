package realtimews

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

func upstream(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "upstream", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUpstreamServerEventsParse(t *testing.T) {
	cases := map[string]func(t *testing.T, ev realtime.Event){
		"response.output_audio.delta": nil, // doc delta is a placeholder, see below
		"response.output_audio_transcript.delta": func(t *testing.T, ev realtime.Event) {
			if ev.Kind != realtime.EventAgentTranscriptDelta || ev.ItemID != "msg_008" || ev.Text != "Hello, how can I a" {
				t.Fatalf("%+v", ev)
			}
		},
		"response.output_audio_transcript.done": func(t *testing.T, ev realtime.Event) {
			if ev.Kind != realtime.EventAgentTranscriptDone || ev.Text != "Hello, how can I assist you today?" {
				t.Fatalf("%+v", ev)
			}
		},
		"conversation.item.input_audio_transcription.completed": func(t *testing.T, ev realtime.Event) {
			if ev.Kind != realtime.EventCallerTranscriptFinal || ev.ItemID != "item_CCXGQ4e1ht4cOraEYcuR2" || ev.Text != "Hey, can you hear me?" || ev.HasTiming {
				t.Fatalf("%+v", ev)
			}
		},
		"conversation.item.input_audio_transcription.delta": func(t *testing.T, ev realtime.Event) {
			if ev.Kind != realtime.EventCallerTranscriptDelta || ev.Text != "Hey" {
				t.Fatalf("%+v", ev)
			}
		},
		"conversation.item.input_audio_transcription.failed": func(t *testing.T, ev realtime.Event) {
			if ev.Kind != realtime.EventCallerTranscriptFailed || ev.ErrCode != "audio_unintelligible" {
				t.Fatalf("%+v", ev)
			}
		},
		"input_audio_buffer.speech_started": func(t *testing.T, ev realtime.Event) {
			if ev.Kind != realtime.EventSpeechStarted || ev.StartMS != 1000 || ev.ItemID != "msg_003" {
				t.Fatalf("%+v", ev)
			}
		},
		"conversation.item.truncated": func(t *testing.T, ev realtime.Event) {
			if ev.Kind != realtime.EventTruncated || ev.ItemID != "msg_004" || ev.EndMS != 1500 || ev.Text != "" {
				t.Fatalf("truncated must carry no transcript: %+v", ev)
			}
		},
		"response.done": func(t *testing.T, ev realtime.Event) {
			if ev.Kind != realtime.EventResponseDone || ev.ResponseStatus != "completed" || ev.UsageTokens != 253 {
				t.Fatalf("%+v", ev)
			}
		},
		"error": func(t *testing.T, ev realtime.Event) {
			if ev.Kind != realtime.EventError || ev.ErrCode != "invalid_event" {
				t.Fatalf("%+v", ev)
			}
		},
		"session.created": func(t *testing.T, ev realtime.Event) {
			if ev.Kind != realtime.EventIgnored {
				t.Fatalf("%+v", ev)
			}
		},
	}
	for name, check := range cases {
		t.Run(name, func(t *testing.T) {
			ev, err := ParseServerEvent(upstream(t, name))
			if name == "response.output_audio.delta" {
				// The reference example's delta is the placeholder text
				// "Base64EncodedAudioDelta" (23 chars, not valid padded base64).
				// It must be rejected; real audio is exercised in the session test.
				if !errors.Is(err, realtime.ErrProtocol) || ev.ItemID != "msg_008" || ev.ResponseID != "resp_001" {
					t.Fatalf("%+v %v", ev, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			check(t, ev)
		})
	}
}

func TestServerEventsNegative(t *testing.T) {
	cases := map[string]string{
		"no type":                  `{"event_id":"x"}`,
		"not json":                 `{`,
		"audio without item":       `{"type":"response.output_audio.delta","response_id":"r","delta":"AAA="}`,
		"audio not base64":         `{"type":"response.output_audio.delta","response_id":"r","item_id":"i","delta":"**"}`,
		"transcript done no text":  `{"type":"response.output_audio_transcript.done","item_id":"i"}`,
		"caller final no item":     `{"type":"conversation.item.input_audio_transcription.completed","transcript":"hi"}`,
		"truncated no end":         `{"type":"conversation.item.truncated","item_id":"i"}`,
		"done no status":           `{"type":"response.done","response":{}}`,
		"beta audio delta":         `{"type":"response.audio.delta","response_id":"r","item_id":"i","delta":"AAA="}`,
		"beta transcript":          `{"type":"response.audio_transcript.delta","item_id":"i","delta":"x"}`,
		"live event on realtime":   `{"type":"session.output_audio.delta","delta":"AAA="}`,
		"live started on realtime": `{"type":"session.started","session":{"id":"x"}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseServerEvent([]byte(raw)); !errors.Is(err, realtime.ErrProtocol) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// fakeRealtime is an independent emulator of the documented Realtime GA
// WebSocket flow. It does not import the adapter's encoders.
type fakeRealtime struct {
	mu       sync.Mutex
	got      []map[string]any
	query    string
	auth     string
	updated  func(update map[string]any) map[string]any
	afterUpd []string
}

func (f *fakeRealtime) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.query, f.auth = r.URL.RawQuery, r.Header.Get("Authorization")
		f.mu.Unlock()
		if r.URL.Query().Get("model") == "" {
			http.Error(w, "missing model", http.StatusBadRequest)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, upstream(t, "session.created"))
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(b, &m) != nil {
				return
			}
			f.mu.Lock()
			f.got = append(f.got, m)
			f.mu.Unlock()
			switch m["type"] {
			case "session.update":
				resp := map[string]any{"type": "session.updated", "event_id": "event_upd"}
				s, _ := m["session"].(map[string]any)
				if s == nil || s["type"] != "realtime" {
					_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"error","event_id":"e","error":{"type":"invalid_request_error","code":"missing_required_parameter","message":"session.type"}}`))
					continue
				}
				resp["session"] = s
				if f.updated != nil {
					resp["session"] = f.updated(s)
				}
				b, _ := json.Marshal(resp)
				_ = c.Write(ctx, websocket.MessageText, b)
				for _, ev := range f.afterUpd {
					_ = c.Write(ctx, websocket.MessageText, []byte(ev))
				}
			case "conversation.item.truncate":
				b, _ := json.Marshal(map[string]any{"type": "conversation.item.truncated", "event_id": "event_tr", "item_id": m["item_id"], "content_index": m["content_index"], "audio_end_ms": m["audio_end_ms"]})
				_ = c.Write(ctx, websocket.MessageText, b)
			}
		}
	}
}

func startFake(t *testing.T, f *fakeRealtime) string {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
}

func cfg(endpoint string) Config {
	return Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Book a table.", Format: audio.Format{Encoding: audio.PCMU, SampleRate: 8000}}
}

func TestSessionHandshakeAndInterruption(t *testing.T) {
	pcmu := make([]byte, 160)
	for i := range pcmu {
		pcmu[i] = audio.LinearToUlaw(int16(i * 50))
	}
	f := &fakeRealtime{afterUpd: []string{
		`{"type":"response.output_audio.delta","event_id":"e1","response_id":"resp_1","item_id":"item_1","output_index":0,"content_index":0,"delta":"` + base64.StdEncoding.EncodeToString(pcmu) + `"}`,
		`{"type":"input_audio_buffer.speech_started","event_id":"e2","audio_start_ms":2300,"item_id":"item_2"}`,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Connect(ctx, cfg(startFake(t, f)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if f.query != "model=gpt-realtime-2.1" || f.auth != "Bearer sk-test" {
		t.Fatalf("query=%q auth=%q", f.query, f.auth)
	}
	if s.SessionID != "sess_C9G5QPteg4UIbotdKLoYQ" {
		t.Fatalf("session id %q", s.SessionID)
	}
	ev, err := s.Next(ctx)
	if err != nil || ev.Kind != realtime.EventAudio || len(ev.Audio) != 160 || ev.ItemID != "item_1" {
		t.Fatalf("%+v %v", ev, err)
	}
	ev, err = s.Next(ctx)
	if err != nil || ev.Kind != realtime.EventSpeechStarted {
		t.Fatalf("%+v %v", ev, err)
	}
	if err := s.Truncate(ctx, "item_1", 0, 20); err != nil {
		t.Fatal(err)
	}
	ev, err = s.Next(ctx)
	if err != nil || ev.Kind != realtime.EventTruncated || ev.EndMS != 20 || ev.ItemID != "item_1" {
		t.Fatalf("%+v %v", ev, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	upd := f.got[0]
	sess := upd["session"].(map[string]any)
	in := sess["audio"].(map[string]any)["input"].(map[string]any)
	if _, has := in["transcription"]; has {
		t.Fatal("billed input transcription must not be enabled implicitly")
	}
	if in["format"].(map[string]any)["type"] != "audio/pcmu" {
		t.Fatalf("format %v", in["format"])
	}
}

func TestSessionFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Run("server ignores format", func(t *testing.T) {
		f := &fakeRealtime{updated: func(s map[string]any) map[string]any {
			s["audio"] = map[string]any{"input": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}}, "output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}}}
			return s
		}}
		if _, err := Connect(ctx, cfg(startFake(t, f))); !errors.Is(err, realtime.ErrCapability) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unsupported format", func(t *testing.T) {
		c := cfg("ws://127.0.0.1:1/v1/realtime")
		c.Format = audio.Format{Encoding: audio.L16, SampleRate: 16000, LittleEndian: true}
		if _, err := Connect(ctx, c); !errors.Is(err, realtime.ErrCapability) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("live server", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			_, _, _ = c.Read(r.Context()) // Live waits for session.start; send nothing first
		}))
		defer srv.Close()
		short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		if _, err := Connect(short, cfg("ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/live/sessions")); err == nil {
			t.Fatal("connected to a server that never sent session.created")
		}
	})
	t.Run("wrong first event", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.started","session":{"id":"live_1"}}`))
			_, _, _ = c.Read(r.Context())
		}))
		defer srv.Close()
		if _, err := Connect(ctx, cfg("ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime")); !errors.Is(err, realtime.ErrProtocol) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("endpoint rules", func(t *testing.T) {
		for _, ep := range []string{"https://api.example/v1/realtime", "ws://api.example/v1/realtime", "wss://u:p@api.example/v1/realtime", "wss://api.example/v1/realtime?model=x"} {
			if _, err := Connect(ctx, cfg(ep)); err == nil {
				t.Errorf("accepted %s", ep)
			}
		}
	})
}

// realtime_ws caller transcription exists only as a separately billed
// transcription model. CallHarbor must not enable it, so a two-party
// transcript requirement fails closed and the V1 caller transcript is
// reported as unavailable (owner decision, option a).
func TestRealtimeTwoPartyTranscriptFailsClosed(t *testing.T) {
	req := realtime.Requirements{Format: audio.Format{Encoding: audio.PCMU, SampleRate: 8000}, TwoPartyTranscript: true}
	if err := realtime.RealtimeCapabilities.Check(req); !errors.Is(err, realtime.ErrCapability) {
		t.Fatalf("realtime_ws must not claim two-party transcript: %v", err)
	}
	if got := realtime.RealtimeCapabilities.V1CallerTranscript(); got != realtime.Unavailable {
		t.Fatalf("realtime_ws V1 caller transcript = %q, want unavailable", got)
	}
	if got := realtime.LiveCapabilities.V1CallerTranscript(); got != realtime.Native {
		t.Fatalf("live_ws V1 caller transcript = %q, want native", got)
	}
	b, err := SessionUpdate(cfg("wss://api.example/v1/realtime"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "transcription") {
		t.Fatalf("session.update must never enable billed transcription: %s", b)
	}
}
