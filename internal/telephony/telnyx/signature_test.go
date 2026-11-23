package telnyx

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"
	"time"
)

// Deterministic vector: RFC 8032 section 7.1 TEST 1 key, signing
// "1700000000|<body>" as Telnyx SDKs do. Telnyx publishes no static vector;
// this one was generated independently with Go crypto/ed25519 and is also
// reproducible with any RFC 8032 implementation.
const (
	vecSeedHex   = "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"
	vecPublicB64 = "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="
	vecTimestamp = "1700000000"
	vecBody      = `{"data":{"event_type":"call.initiated","id":"0ccc7b54-4df3-4bca-a65a-3da1ecc777f0","payload":{"call_control_id":"v2:abc"},"record_type":"event"}}`
	vecBodySig   = "mZwebqNVaHsULDq3CV/Nzw2rV4U3CRfACZiDCY2Fx29fH5AV4pz4yw0WbXhqJeNdQs5NzEd/SHnM1hVVBLycDw=="
	vecStreamURL = "wss://example.test/media?token=abc"
	vecStreamSig = "jqWcZiJ0ZgMX9Fg95YvkGYcU1jST5xPu/Ph/a0cfFvnnwYuDT66mbNCYuXC9uM0lClizWQpE9x6oEeo/k4dwAA=="
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

func newVecVerifier(t *testing.T, at time.Time) (*Verifier, *fakeClock) {
	t.Helper()
	k, err := ParsePublicKey(vecPublicB64)
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: at}
	v, err := NewVerifier(k, DefaultTolerance, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	return v, clk
}

func hdr(ts, sig string) http.Header {
	h := http.Header{}
	if ts != "" {
		h.Set("telnyx-timestamp", ts)
	}
	if sig != "" {
		h.Set("telnyx-signature-ed25519", sig)
	}
	return h
}

func TestVectorKeyDerivation(t *testing.T) {
	seed, _ := hex.DecodeString(vecSeedHex)
	priv := ed25519.NewKeyFromSeed(seed)
	k, _ := ParsePublicKey(vecPublicB64)
	if !priv.Public().(ed25519.PublicKey).Equal(k) {
		t.Fatal("vector public key does not match RFC 8032 seed")
	}
	got := Sign(priv, time.Unix(1700000000, 0), []byte(vecBody)).Get(HeaderSignature)
	if got != vecBodySig {
		t.Fatalf("emitter signature %s != vector", got)
	}
}

func TestVerifyWebhookVector(t *testing.T) {
	v, _ := newVecVerifier(t, time.Unix(1700000000, 0).Add(30*time.Second))
	if err := v.VerifyWebhook(hdr(vecTimestamp, vecBodySig), []byte(vecBody)); err != nil {
		t.Fatalf("valid vector rejected: %v", err)
	}
}

func TestVerifyWebhookNegative(t *testing.T) {
	base := time.Unix(1700000000, 0)
	cases := []struct {
		name string
		now  time.Time
		h    http.Header
		body string
		want error
	}{
		{"missing signature", base, hdr(vecTimestamp, ""), vecBody, ErrMissingSignature},
		{"missing timestamp", base, hdr("", vecBodySig), vecBody, ErrMissingSignature},
		{"body reserialized", base, hdr(vecTimestamp, vecBodySig), vecBody + "\n", ErrBadSignature},
		{"body field reordered", base, hdr(vecTimestamp, vecBodySig), `{"data":{"id":"0ccc7b54-4df3-4bca-a65a-3da1ecc777f0","event_type":"call.initiated","payload":{"call_control_id":"v2:abc"},"record_type":"event"}}`, ErrBadSignature},
		{"timestamp swapped", base, hdr("1700000001", vecBodySig), vecBody, ErrBadSignature},
		{"stream sig on body", base, hdr(vecTimestamp, vecStreamSig), vecBody, ErrBadSignature},
		{"too old", base.Add(DefaultTolerance + time.Second), hdr(vecTimestamp, vecBodySig), vecBody, ErrStaleTimestamp},
		{"future", base.Add(-DefaultTolerance - time.Second), hdr(vecTimestamp, vecBodySig), vecBody, ErrStaleTimestamp},
		{"signed timestamp", base, hdr("+1700000000", vecBodySig), vecBody, ErrMalformedHeader},
		{"ms timestamp", base, hdr("1700000000000", vecBodySig), vecBody, ErrStaleTimestamp},
		{"base64url sig", base, hdr(vecTimestamp, "mZwebqNVaHsULDq3CV_Nzw2rV4U3CRfACZiDCY2Fx29fH5AV4pz4yw0WbXhqJeNdQs5NzEd_SHnM1hVVBLycDw=="), vecBody, ErrMalformedHeader},
		{"short sig", base, hdr(vecTimestamp, "AAAA"), vecBody, ErrMalformedHeader},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := newVecVerifier(t, tc.now)
			err := v.VerifyWebhook(tc.h, []byte(tc.body))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyWebhookReplay(t *testing.T) {
	v, clk := newVecVerifier(t, time.Unix(1700000000, 0))
	if err := v.VerifyWebhook(hdr(vecTimestamp, vecBodySig), []byte(vecBody)); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(4 * time.Minute)
	if err := v.VerifyWebhook(hdr(vecTimestamp, vecBodySig), []byte(vecBody)); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay within window: %v", err)
	}
	clk.t = clk.t.Add(2 * time.Minute)
	if err := v.VerifyWebhook(hdr(vecTimestamp, vecBodySig), []byte(vecBody)); !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("replay after window must fail as stale: %v", err)
	}
}

func TestVerifyMediaHandshakeVector(t *testing.T) {
	v, _ := newVecVerifier(t, time.Unix(1700000000, 0))
	if err := v.VerifyMediaHandshake(hdr(vecTimestamp, vecStreamSig), vecStreamURL); err != nil {
		t.Fatalf("valid handshake rejected: %v", err)
	}
	for name, u := range map[string]string{
		"query dropped":  "wss://example.test/media",
		"query changed":  "wss://example.test/media?token=abd",
		"scheme changed": "ws://example.test/media?token=abc",
		"trailing slash": "wss://example.test/media/?token=abc",
	} {
		t.Run(name, func(t *testing.T) {
			v, _ := newVecVerifier(t, time.Unix(1700000000, 0))
			if err := v.VerifyMediaHandshake(hdr(vecTimestamp, vecStreamSig), u); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestParsePublicKeyRejects(t *testing.T) {
	for _, k := range []string{"", "not base64!", "AAAA", "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo", "-----BEGIN PUBLIC KEY-----"} {
		if _, err := ParsePublicKey(k); err == nil {
			t.Errorf("accepted key %q", k)
		}
	}
}
