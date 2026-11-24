package telnyxfake

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

func newKeys(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

type mediaServer struct {
	*httptest.Server
	streamURL string
}

func newMediaServer(t *testing.T, pub ed25519.PublicKey, now time.Time, reply []byte) *mediaServer {
	t.Helper()
	v, err := telnyx.NewVerifier(pub, telnyx.DefaultTolerance, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ms := &mediaServer{}
	ms.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := v.VerifyMediaHandshake(r.Header, ms.streamURL); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx := r.Context()
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		_ = c.Write(ctx, websocket.MessageText, reply)
	}))
	ms.streamURL = "ws" + strings.TrimPrefix(ms.URL, "http") + "/media?token=abc"
	t.Cleanup(ms.Close)
	return ms
}

func TestDialMediaSignedRoundTrip(t *testing.T) {
	priv, pub := newKeys(t)
	now := time.Now()
	want := []byte(`{"event":"clear"}`)
	srv := newMediaServer(t, pub, now, want)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := DialMedia(ctx, srv.streamURL, priv, now)
	if err != nil {
		t.Fatalf("signed dial rejected: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Send(ctx, []byte(`{"event":"start"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestDialMediaRejectsWrongKey(t *testing.T) {
	_, pub := newKeys(t)
	otherPriv, _ := newKeys(t)
	now := time.Now()
	srv := newMediaServer(t, pub, now, []byte(`{}`))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := DialMedia(ctx, srv.streamURL, otherPriv, now); err == nil {
		t.Fatal("handshake signed with the wrong key was accepted")
	}
}

func TestDialMediaRejectsChangedURL(t *testing.T) {
	priv, pub := newKeys(t)
	now := time.Now()
	srv := newMediaServer(t, pub, now, []byte(`{}`))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tampered := srv.streamURL + "&x=1"
	if _, err := DialMedia(ctx, tampered, priv, now); err == nil {
		t.Fatal("handshake for a different stream URL was accepted")
	}
}
