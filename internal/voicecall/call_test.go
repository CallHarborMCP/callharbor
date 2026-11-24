package voicecall_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/realtime/livews"
	"github.com/CallHarborMCP/callharbor/internal/realtime/realtimews"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx/telnyxfake"
	"github.com/CallHarborMCP/callharbor/internal/voice"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
	"github.com/CallHarborMCP/callharbor/internal/voicecall"
)

const providerKey = "test-provider-key-not-real"

var (
	pcmu = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}
	when = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
)

// fakeProvider records the upgrade headers and the caller audio it receives.
type fakeProvider struct {
	mu      sync.Mutex
	authz   string
	inbound [][]byte
	seen    chan struct{}
	once    sync.Once
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{seen: make(chan struct{})}
}

func (f *fakeProvider) setAuth(v string) {
	f.mu.Lock()
	f.authz = v
	f.mu.Unlock()
}

func (f *fakeProvider) auth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authz
}

func (f *fakeProvider) noteAudio(raw []byte) {
	f.mu.Lock()
	f.inbound = append(f.inbound, raw)
	f.mu.Unlock()
	f.once.Do(func() { close(f.seen) })
}

func (f *fakeProvider) received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.inbound...)
}

// realtimeHandler speaks the OpenAI Realtime WebSocket shape the adapter uses.
func (f *fakeProvider) realtimeHandler(agent []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.setAuth(r.Header.Get("Authorization"))
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
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created","event_id":"evt_created","session":{"id":"sess_it","type":"realtime"}}`))
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(b, &m) != nil {
				return
			}
			switch m["type"] {
			case "session.update":
				s, _ := m["session"].(map[string]any)
				upd, _ := json.Marshal(map[string]any{"type": "session.updated", "event_id": "evt_upd", "session": s})
				_ = c.Write(ctx, websocket.MessageText, upd)
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.output_audio.delta","event_id":"evt_d","response_id":"resp_it","item_id":"item_it","output_index":0,"content_index":0,"delta":"`+base64.StdEncoding.EncodeToString(agent)+`"}`))
			case "input_audio_buffer.append":
				raw, err := base64.StdEncoding.DecodeString(m["audio"].(string))
				if err == nil {
					f.noteAudio(raw)
				}
			}
		}
	}
}

// liveHandler speaks the GPT-Live session.start shape the adapter uses.
func (f *fakeProvider) liveHandler(agent []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.setAuth(r.Header.Get("Authorization"))
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx := r.Context()
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(b, &m) != nil {
				return
			}
			switch m["type"] {
			case "session.start":
				s, _ := m["session"].(map[string]any)
				s["id"], s["status"] = "live_it", "active"
				sess, _ := json.Marshal(s)
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.started","event_id":"evt_s","client_event_id":"ch_session_start","session":`+string(sess)+`}`))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.output_audio.delta","delta":"`+base64.StdEncoding.EncodeToString(agent)+`"}`))
			case "session.input_audio.append":
				raw, err := base64.StdEncoding.DecodeString(m["audio"].(string))
				if err == nil {
					f.noteAudio(raw)
				}
			case "session.close":
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.closed","event_id":"evt_c","reason":"close_requested","usage":{"seconds":1}}`))
			}
		}
	}
}

type fixture struct {
	store   *storage.Store
	secrets *secrets.Service
	aliceID string
	bobID   string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "call.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	idSvc, err := identity.NewService(store, func() time.Time { return when })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idSvc.Setup(ctx, "root", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	rootLogin, err := idSvc.Login(ctx, "root", "correct horse battery", "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	root, err := idSvc.Authenticate(ctx, rootLogin.Token)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := idSvc.CreateUser(ctx, root, "alice", "staple battery horse", identity.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := idSvc.CreateUser(ctx, root, "bob", "staple battery horse", identity.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, secrets.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyring(1, map[int][]byte{1: key})
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.NewBox(ring)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	svc, err := secrets.NewService(box, store, func() time.Time { return when }, func() string { n++; return fmt.Sprintf("sec_%d", n) })
	if err != nil {
		t.Fatal(err)
	}
	return fixture{store: store, secrets: svc, aliceID: alice.ID, bobID: bob.ID}
}

func (f fixture) otherID(t *testing.T) string {
	t.Helper()
	return f.bobID
}

// aliceSecretID returns the id of alice's ai_api_key secret, created by
// addProfile under the name "<profile>-key".
func (f fixture) aliceSecretID(t *testing.T) string {
	t.Helper()
	p, err := f.store.GetAIProfile(context.Background(), f.aliceID, "ai-alice")
	if err != nil {
		t.Fatal(err)
	}
	return p.APISecretID
}

// tryBindForeignSecret attempts to create a profile owned by owner whose
// api_secret_id points at another owner's secret. It returns the storage error;
// the composite foreign key must refuse the cross-tenant reference.
func (f fixture) tryBindForeignSecret(t *testing.T, owner, id, foreignSecretID string) error {
	t.Helper()
	ts := when.Format("2006-01-02T15:04:05.000Z")
	return f.store.CreateAIProfile(context.Background(), storage.AIProfile{
		ID: id, OwnerUserID: owner, Name: id, Protocol: storage.AIProtocolRealtime,
		EndpointURL: "wss://ai.example.test/v1", Model: "m", Voice: "v", APISecretID: foreignSecretID,
		CreatedAt: ts, UpdatedAt: ts,
	})
}

// addProfile stores an AI profile and its encrypted key for owner.
func (f fixture) addProfile(t *testing.T, owner, id string, proto storage.AIProtocol, endpoint, key string) {
	t.Helper()
	ctx := context.Background()
	secretID, err := f.secrets.Put(ctx, owner, id+"-key", secrets.PurposeAIAPIKey, []byte(key))
	if err != nil {
		t.Fatal(err)
	}
	ts := when.Format("2006-01-02T15:04:05.000Z")
	if err := f.store.CreateAIProfile(ctx, storage.AIProfile{
		ID: id, OwnerUserID: owner, Name: id, Protocol: proto, EndpointURL: endpoint,
		Model: "voice-model", Voice: "alloy", Instructions: "Be brief.", APISecretID: secretID,
		CreatedAt: ts, UpdatedAt: ts,
	}); err != nil {
		t.Fatal(err)
	}
}

func testAudio(seed int) (agent, caller []byte) {
	agent = make([]byte, 160)
	caller = make([]byte, 160)
	for i := range agent {
		agent[i] = audio.LinearToUlaw(int16(i * (96 + seed)))
		caller[i] = audio.LinearToUlaw(int16(i * (64 + seed)))
	}
	return agent, caller
}

func mediaFrame(track string, payload []byte, ts int) []byte {
	b, _ := json.Marshal(map[string]any{
		"event": "media", "stream_id": "it-1", "sequence_number": fmt.Sprint(ts),
		"media": map[string]any{"track": track, "chunk": "1", "timestamp": fmt.Sprint(ts), "payload": base64.StdEncoding.EncodeToString(payload)},
	})
	return b
}

// TestAICallPathDrivesVoiceRunOverSignedTelnyxMedia wires the whole path. An
// owner's AI profile and encrypted key are resolved, the real dialer connects
// to the fake provider, voice.Run moves audio both ways through the signed
// Telnyx media endpoint, and the fake provider must see the bearer key.
func TestAICallPathDrivesVoiceRunOverSignedTelnyxMedia(t *testing.T) {
	cases := []struct {
		name     string
		proto    storage.AIProtocol
		dialer   realtime.Dialer
		protocol realtime.Protocol
		handler  func(*fakeProvider, []byte) http.HandlerFunc
	}{
		{"realtime_ws", storage.AIProtocolRealtime, realtimews.Dialer, realtime.ProtocolRealtime,
			func(f *fakeProvider, agent []byte) http.HandlerFunc { return f.realtimeHandler(agent) }},
		{"live_ws", storage.AIProtocolLive, livews.Dialer, realtime.ProtocolLive,
			func(f *fakeProvider, agent []byte) http.HandlerFunc { return f.liveHandler(agent) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent, caller := testAudio(0)
			fp := newFakeProvider()
			provider := httptest.NewServer(tc.handler(fp, agent))
			t.Cleanup(provider.Close)
			endpoint := "ws" + strings.TrimPrefix(provider.URL, "http") + "/v1/voice"

			f := newFixture(t)
			f.addProfile(t, f.aliceID, "ai-"+tc.name, tc.proto, endpoint, providerKey)

			resolver, err := voicecall.NewResolver(f.store, f.secrets, pcmu, realtime.EndpointPolicy{AllowPrivate: true})
			if err != nil {
				t.Fatal(err)
			}
			dialers := realtime.Dialers{realtime.ProtocolRealtime: realtimews.Dialer, realtime.ProtocolLive: livews.Dialer}

			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			v, err := telnyx.NewVerifier(pub, telnyx.DefaultTolerance, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			result := make(chan error, 1)
			var me *telnyx.MediaEndpoint
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { me.ServeHTTP(w, r) }))
			t.Cleanup(srv.Close)
			streamURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/media?token=it"

			me, err = telnyx.NewMediaEndpoint(v, streamURL, func(start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
				bridge, err := voicebridge.New(start.Format, pcmu, 5*time.Millisecond, 2*time.Second)
				if err != nil {
					return nil, err
				}
				sink, err := telnyx.NewBridgeSink(bridge, send)
				if err != nil {
					return nil, err
				}
				ab, err := voicecall.NewAudioBridge(sink)
				if err != nil {
					return nil, err
				}
				prof, err := resolver.Resolve(ctx, f.aliceID, "ai-"+tc.name)
				if err != nil {
					return nil, err
				}
				sess, err := dialers.Dial(ctx, prof, realtime.Requirements{Format: pcmu})
				if err != nil {
					return nil, err
				}
				go func() {
					_, err := voice.Run(ctx, sess, ab, ab.EventSink(nil))
					result <- err
				}()
				return sink, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			s, err := telnyxfake.DialMedia(ctx, streamURL, priv, now)
			if err != nil {
				t.Fatalf("signed media dial: %v", err)
			}
			defer func() { _ = s.Close() }()
			start := []byte(`{"event":"start","stream_id":"it-1","sequence_number":"1","start":{"call_control_id":"v3:it","client_state":"","media_format":{"encoding":"PCMU","sample_rate":8000,"channels":1}}}`)
			if err := s.Send(ctx, start); err != nil {
				t.Fatal(err)
			}
			if err := s.Send(ctx, mediaFrame("inbound", caller, 320)); err != nil {
				t.Fatal(err)
			}

			got, err := s.Recv(ctx)
			if err != nil {
				t.Fatalf("no agent media from voice.Run: %v", err)
			}
			var out struct {
				Event string `json:"event"`
				Media struct {
					Payload string `json:"payload"`
				} `json:"media"`
			}
			if err := json.Unmarshal(got, &out); err != nil || out.Event != "media" {
				t.Fatalf("want agent media frame, got %s", got)
			}
			gotAgent, err := base64.StdEncoding.DecodeString(out.Media.Payload)
			if err != nil || !bytes.Equal(gotAgent, agent) {
				t.Fatalf("agent audio not byte-exact through Telnyx: %v", err)
			}

			select {
			case <-fp.seen:
			case <-ctx.Done():
				t.Fatal("provider received no caller audio")
			}
			if in := fp.received(); len(in) == 0 || !bytes.Equal(in[0], caller) {
				t.Fatal("caller audio not byte-exact to provider")
			}

			if want := "Bearer " + providerKey; fp.auth() != want {
				t.Fatalf("provider Authorization = %q, want %q", fp.auth(), want)
			}

			cancel()
			if err := <-result; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, voicecall.ErrBridgeClosed) {
				t.Fatalf("voice.Run: %v", err)
			}
		})
	}
}
