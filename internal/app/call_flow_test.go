package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/observability"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx/telnyxfake"
)

// callTestEnv is one real app instance on a TLS origin. The origin is the
// public URL, so webhook and stream URLs are https and wss, as production
// requires. The test CA is the only trust anchor; no verification is skipped.
type callTestEnv struct {
	t       *testing.T
	base    string
	client  *http.Client
	admin   *session
	pubKey  ed25519.PublicKey
	privKey ed25519.PrivateKey
	roots   *x509.CertPool
	tel     *fakeTelnyx
	dataDir string
	// stop shuts the app down through run's context and waits for it.
	stop func()
}

// webhookKeyB64 is the raw Ed25519 public key, base64 encoded, that the
// telephony profile stores as its webhook public key.
func (e *callTestEnv) webhookKeyB64() string {
	return base64.StdEncoding.EncodeToString(e.pubKey)
}

// newTLSListener serves on loopback with a self-signed certificate for
// 127.0.0.1 and returns the listener plus a root pool that trusts that cert.
func newTLSListener(t *testing.T) (net.Listener, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "callharbor-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}), pool
}

// startCallApp boots the real app on a TLS listener with a master key. The
// Telnyx REST root and the AI endpoints are the fakes; the endpoint policy
// allows loopback, which the fakes need. Shutdown is registered with t.Cleanup.
func startCallApp(t *testing.T, tel *fakeTelnyx, aiDialers realtime.Dialers) *callTestEnv {
	t.Helper()
	return startCallAppWith(t, tel, aiDialers, nil)
}

// startCallAppWith is startCallApp with a hook to adjust the config first.
func startCallAppWith(t *testing.T, tel *fakeTelnyx, aiDialers realtime.Dialers, adjust func(*config.Config)) *callTestEnv {
	t.Helper()
	t.Setenv(secrets.EnvKey, validKeyB64())
	ln, roots := newTLSListener(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.PublicURL = "https://" + ln.Addr().String()
	if adjust != nil {
		adjust(&cfg)
	}
	opts := runOptions{
		TelnyxBaseURL:  tel.baseURL(),
		EndpointPolicy: realtime.EndpointPolicy{AllowPrivate: true},
		AIDialers:      aiDialers,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, observability.NewLogger(io.Discard, slog.LevelError), ln, opts)
	}()
	// stop cancels run and waits for it. It is idempotent, so a test can stop
	// the app itself and cleanup can still call it.
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("run = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("run did not return after cancel")
			}
		})
	}
	t.Cleanup(stop)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	waitForStatus(t, client, cfg.PublicURL+"/readyz", http.StatusOK)
	env := &callTestEnv{t: t, base: cfg.PublicURL, client: client, pubKey: pub, privKey: priv, roots: roots, tel: tel, dataDir: cfg.DataDir, stop: stop}
	status, _, body := sendJSON(t, client, http.MethodPost, cfg.PublicURL+"/api/setup",
		map[string]string{"username": "admin", "password": e2ePassword}, nil)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("setup = %d %s", status, body)
	}
	env.admin = login(t, client, cfg.PublicURL, "admin")
	return env
}

// signedWebhook sends one Telnyx-signed call event to the call's webhook route.
// The signature covers the exact body, as Telnyx signs it.
func (e *callTestEnv) signedWebhook(callID, eventType, eventID, controlID string) {
	e.t.Helper()
	// Telnyx returns the dial's client_state on every event for the call. The
	// dial sends the call id, so the fixture echoes it the same way.
	clientState := base64.StdEncoding.EncodeToString([]byte(callID))
	body, err := json.Marshal(map[string]any{"data": map[string]any{
		"record_type": "event", "id": eventID, "event_type": eventType,
		"occurred_at": "2026-11-25T10:00:00Z",
		"payload":     map[string]any{"call_control_id": controlID, "client_state": clientState},
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	h := telnyx.Sign(e.privKey, time.Now(), body)
	h.Set("Content-Type", "application/json")
	status, _, resp := send(e.t, e.client, http.MethodPost,
		e.base+"/telnyx/calls/"+callID+"/webhook", body, h)
	if status != http.StatusOK {
		e.t.Fatalf("webhook %s = %d %s", eventType, status, resp)
	}
}

// dialedStreamURL returns the stream_url from the last dial the fake received.
// The media handshake must sign this exact string.
func (e *callTestEnv) dialedStreamURL() string {
	e.t.Helper()
	dials := e.tel.dialBodies()
	if len(dials) == 0 {
		e.t.Fatal("no dial reached the fake Telnyx")
	}
	s, _ := dials[len(dials)-1]["stream_url"].(string)
	if s == "" {
		e.t.Fatal("dial body has no stream_url")
	}
	return s
}

// signedMediaDial opens a Telnyx-signed media WebSocket to streamURL. It trusts
// only the test CA.
func (e *callTestEnv) signedMediaDial(streamURL string) (*telnyxfake.Stream, error) {
	return e.signedMediaDialAt(streamURL, time.Now())
}

// signedMediaDialAt is signedMediaDial with an explicit signing time. The app
// refuses a repeated handshake signature (replay protection), so two streams to
// the same URL must be signed at different instants, as Telnyx does per dial.
func (e *callTestEnv) signedMediaDialAt(streamURL string, at time.Time) (*telnyxfake.Stream, error) {
	hc := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: e.roots, MinVersion: tls.VersionTLS12},
	}}
	return telnyxfake.DialMedia(context.Background(), streamURL, e.privKey, at, telnyxfake.WithHTTPClient(hc))
}
