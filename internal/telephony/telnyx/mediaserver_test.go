package telnyx_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx/telnyxfake"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
)

var g711 = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

type recorder struct {
	mu     sync.Mutex
	inputs [][]byte
	dtmf   []string
	errs   []telnyx.Frame
}

type harness struct {
	srv       *httptest.Server
	streamURL string
	priv      ed25519.PrivateKey
	rec       *recorder
}

func newHarness(t *testing.T, priv ed25519.PrivateKey, pub ed25519.PublicKey) *harness {
	t.Helper()
	h := &harness{rec: &recorder{}, priv: priv}
	v, err := telnyx.NewVerifier(pub, telnyx.DefaultTolerance, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var ep http.Handler
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ep.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	h.streamURL = "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/media?token=abc"
	me, err := telnyx.NewMediaEndpoint(v, h.streamURL, func(start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
		b, err := voicebridge.New(start.Format, g711, time.Millisecond, time.Second)
		if err != nil {
			return nil, err
		}
		s, err := telnyx.NewBridgeSink(b, send)
		if err != nil {
			return nil, err
		}
		s.ToProvider = func(p []byte) error {
			h.rec.mu.Lock()
			defer h.rec.mu.Unlock()
			h.rec.inputs = append(h.rec.inputs, p)
			return nil
		}
		s.OnDTMF = func(d string) {
			h.rec.mu.Lock()
			defer h.rec.mu.Unlock()
			h.rec.dtmf = append(h.rec.dtmf, d)
		}
		s.OnError = func(f telnyx.Frame) {
			h.rec.mu.Lock()
			defer h.rec.mu.Unlock()
			h.rec.errs = append(h.rec.errs, f)
		}
		return s, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ep = me
	return h
}

func keys(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func startFrame(t *testing.T) []byte {
	return mustJSON(t, map[string]any{
		"event": "start", "stream_id": "s1",
		"start": map[string]any{
			"call_control_id": "cc1",
			"media_format":    map[string]any{"encoding": "PCMU", "sample_rate": 8000, "channels": 1},
		},
	})
}

func endpointMedia(t *testing.T, track string, payload []byte) []byte {
	return mustJSON(t, map[string]any{
		"event": "media", "stream_id": "s1",
		"media": map[string]any{"track": track, "chunk": "1", "timestamp": "160",
			"payload": base64.StdEncoding.EncodeToString(payload)},
	})
}

func dial(t *testing.T, h *harness) *telnyxfake.Stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := telnyxfake.DialMedia(ctx, h.streamURL, h.priv, time.Now())
	if err != nil {
		t.Fatalf("signed dial rejected: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMediaEndpointBridgesCallerAudioAndEvents(t *testing.T) {
	priv, pub := keys(t)
	h := newHarness(t, priv, pub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := dial(t, h)

	pcmu := []byte{0xff, 0x80, 0x10, 0x20}
	for _, f := range [][]byte{
		[]byte(`{"event":"connected","version":"1.0.0"}`),
		startFrame(t),
		endpointMedia(t, "inbound", pcmu),
		mustJSON(t, map[string]any{"event": "dtmf", "stream_id": "s1", "dtmf": map[string]any{"digit": "5"}}),
		mustJSON(t, map[string]any{"event": "error", "stream_id": "s1", "payload": map[string]any{"code": 40010, "title": "t"}}),
		mustJSON(t, map[string]any{"event": "stop", "stream_id": "s1"}),
	} {
		if err := s.Send(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Recv(ctx); err == nil {
		t.Fatal("server should close the stream after stop")
	}

	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	if len(h.rec.inputs) != 1 || string(h.rec.inputs[0]) != string(pcmu) {
		t.Fatalf("caller audio not bridged byte-exact: %v", h.rec.inputs)
	}
	if len(h.rec.dtmf) != 1 || h.rec.dtmf[0] != "5" {
		t.Fatalf("dtmf: %v", h.rec.dtmf)
	}
	if len(h.rec.errs) != 1 || h.rec.errs[0].ErrCode != 40010 {
		t.Fatalf("error frame: %+v", h.rec.errs)
	}
}

func TestMediaEndpointRejectsMediaBeforeStart(t *testing.T) {
	priv, pub := keys(t)
	h := newHarness(t, priv, pub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := dial(t, h)
	if err := s.Send(ctx, endpointMedia(t, "inbound", []byte{1})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(ctx); err == nil {
		t.Fatal("media before start was accepted")
	}
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	if len(h.rec.inputs) != 0 {
		t.Fatal("media before start reached the provider")
	}
}

func TestMediaEndpointRejectsMalformedFrame(t *testing.T) {
	priv, pub := keys(t)
	h := newHarness(t, priv, pub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := dial(t, h)
	if err := s.Send(ctx, []byte(`{"event":"start","stream_id":"s1"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(ctx); err == nil {
		t.Fatal("malformed start was accepted")
	}
}

func TestMediaEndpointRejectsUnsignedHandshake(t *testing.T) {
	_, pub := keys(t)
	otherPriv, _ := keys(t)
	h := newHarness(t, otherPriv, pub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := telnyxfake.DialMedia(ctx, h.streamURL, otherPriv, time.Now()); err == nil {
		t.Fatal("handshake signed with the wrong key reached the endpoint")
	}
}

func TestMediaEndpointHTTPRejectWithoutSignature(t *testing.T) {
	_, pub := keys(t)
	priv, _ := keys(t)
	h := newHarness(t, priv, pub)
	resp, err := http.Get(h.srv.URL + "/media")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned request status %d", resp.StatusCode)
	}
}
