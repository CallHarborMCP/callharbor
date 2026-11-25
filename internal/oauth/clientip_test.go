package oauth

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// proxyTrusted is the trusted list the proxy tests use: one private /24 that
// stands for the reverse proxy's network.
var proxyTrusted = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}

// clientIPReq builds a request from peer with the given X-Forwarded-For values.
func clientIPReq(peer string, xff ...string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", nil)
	req.RemoteAddr = peer
	for _, v := range xff {
		req.Header.Add("X-Forwarded-For", v)
	}
	return req
}

func TestClientIPWithoutTrustedProxyUsesPeer(t *testing.T) {
	req := clientIPReq("203.0.113.9:4000", "192.0.2.99")
	if got := registrationClientIP(req, nil); got != "203.0.113.9" {
		t.Fatalf("key = %q, want the peer 203.0.113.9", got)
	}
}

// TestClientIPUntrustedPeerCannotEscapeBucket: a peer outside the trusted list
// that sends X-Forwarded-For keeps its own key, whatever the header says.
func TestClientIPUntrustedPeerCannotEscapeBucket(t *testing.T) {
	for _, xff := range []string{"192.0.2.1", "192.0.2.1, 192.0.2.2", "not-an-ip"} {
		req := clientIPReq("203.0.113.9:4000", xff)
		if got := registrationClientIP(req, proxyTrusted); got != "203.0.113.9" {
			t.Fatalf("XFF %q: key = %q, want the untrusted peer", xff, got)
		}
	}
}

// TestClientIPTrustedProxyPicksRightmostUntrusted: behind a trusted proxy the
// chain is read right to left, and the first entry not in a trusted prefix is
// the client. Entries the proxy itself appended to the right are skipped.
func TestClientIPTrustedProxyPicksRightmostUntrusted(t *testing.T) {
	cases := []struct {
		name string
		xff  []string
		want string
	}{
		{"single client", []string{"198.51.100.7"}, "198.51.100.7"},
		{"client then proxy hop", []string{"198.51.100.7, 10.0.0.5"}, "198.51.100.7"},
		{"forged left entry ignored", []string{"192.0.2.66, 198.51.100.7"}, "198.51.100.7"},
		{"multi-hop chain", []string{"192.0.2.66, 198.51.100.7, 10.0.0.5, 10.0.0.6"}, "198.51.100.7"},
		{"values split across headers", []string{"192.0.2.66", "198.51.100.7, 10.0.0.5"}, "198.51.100.7"},
		{"ipv6 client keys on /64", []string{"2001:db8:1:2:aaaa:bbbb:cccc:dddd"}, "2001:db8:1:2::/64"},
		{"mapped client keys as ipv4", []string{"::ffff:198.51.100.7"}, "198.51.100.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := clientIPReq("10.0.0.2:4000", tc.xff...)
			if got := registrationClientIP(req, proxyTrusted); got != tc.want {
				t.Fatalf("key = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientIPTwoClientsBehindOneProxyGetSeparateKeys: the two callers have
// different XFF entries, so they key differently.
func TestClientIPTwoClientsBehindOneProxyGetSeparateKeys(t *testing.T) {
	a := registrationClientIP(clientIPReq("10.0.0.2:4000", "198.51.100.7"), proxyTrusted)
	b := registrationClientIP(clientIPReq("10.0.0.2:4000", "198.51.100.8"), proxyTrusted)
	if a == b {
		t.Fatalf("two clients share key %q", a)
	}
}

// TestClientIPMalformedForwardedForFallsBackToPeer: any entry the walk reaches
// that is empty, zoned, or not an IP makes the header unusable, so the peer is
// used. Only the entries the walk reaches matter, so a bad entry to the left of
// a usable rightmost client does not stop it being used.
func TestClientIPMalformedForwardedForFallsBackToPeer(t *testing.T) {
	malformed := []string{
		"",
		"198.51.100.7, ",
		"198.51.100.7,,10.0.0.5",
		"garbage",
		"198.51.100.7, garbage",
		"fe80::1%eth0",
		"10.0.0.5, 10.0.0.6",
		"198.51.100.7, fe80::1%eth0, 10.0.0.5",
	}
	for _, xff := range malformed {
		req := clientIPReq("10.0.0.2:4000", xff)
		if got := registrationClientIP(req, proxyTrusted); got != "10.0.0.2" {
			t.Fatalf("XFF %q: key = %q, want the trusted peer 10.0.0.2", xff, got)
		}
	}
}

// TestClientIPIgnoresRealIPAndForwarded: only X-Forwarded-For is read. Other
// client-address headers never change the key, even from a trusted peer.
func TestClientIPIgnoresRealIPAndForwarded(t *testing.T) {
	req := clientIPReq("10.0.0.2:4000")
	req.Header.Set("X-Real-IP", "198.51.100.7")
	req.Header.Set("Forwarded", "for=198.51.100.7")
	if got := registrationClientIP(req, proxyTrusted); got != "10.0.0.2" {
		t.Fatalf("key = %q, want the peer; X-Real-IP and Forwarded must be ignored", got)
	}
}

// TestClientIPWithMissingXFFFallsBackToPeer: a trusted peer that sent no header
// still keys on the peer.
func TestClientIPWithMissingXFFFallsBackToPeer(t *testing.T) {
	req := clientIPReq("10.0.0.2:4000")
	if got := registrationClientIP(req, proxyTrusted); got != "10.0.0.2" {
		t.Fatalf("key = %q, want 10.0.0.2", got)
	}
	if strings.Contains(registrationClientIP(req, proxyTrusted), "/") {
		t.Fatal("IPv4 peer key must be a bare address")
	}
}

// proxyFixture is a fixture whose server trusts proxyTrusted for X-Forwarded-For.
func proxyFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	cfg := f.cfg
	cfg.TrustedProxies = proxyTrusted
	f.serve(newServer(cfg, f.store, registeredResolver{f.store}, f.clock.Now, f.srv.logger, registerGlobalCap))
	return f
}

// registerFrom posts a registration from peer with X-Forwarded-For xff.
func (f *fixture) registerFrom(t *testing.T, peer, xff string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(registrationBody(nil)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", xff)
	req.RemoteAddr = peer
	rec := httptest.NewRecorder()
	f.srv.handleRegister(rec, req)
	return rec.Code
}

// authorizeFrom sends one authorize request from peer with X-Forwarded-For xff
// and reports whether the limiter let it through (any non-429 answer).
func (f *fixture) authorizeFrom(t *testing.T, peer, xff string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize", nil)
	req.Header.Set("X-Forwarded-For", xff)
	req.RemoteAddr = peer
	rec := httptest.NewRecorder()
	f.srv.handleAuthorize(rec, req)
	return rec.Code
}

// TestTrustedProxyClientsHaveSeparateRegisterBuckets: two callers behind one
// trusted proxy each get their own register budget. Exhausting one leaves the
// other free, and the exhausted one is refused.
func TestTrustedProxyClientsHaveSeparateRegisterBuckets(t *testing.T) {
	f := proxyFixture(t)
	const proxy = "10.0.0.2:4000"
	for i := 0; i < registerPerWindow; i++ {
		if code := f.registerFrom(t, proxy, "198.51.100.7"); code != http.StatusCreated {
			t.Fatalf("client A registration %d = %d, want 201", i+1, code)
		}
	}
	if code := f.registerFrom(t, proxy, "198.51.100.7"); code != http.StatusTooManyRequests {
		t.Fatalf("client A over budget = %d, want 429", code)
	}
	if code := f.registerFrom(t, proxy, "198.51.100.8"); code != http.StatusCreated {
		t.Fatalf("client B shares A's bucket: status = %d, want 201", code)
	}
}

// TestTrustedProxyClientsHaveSeparateAuthorizeBuckets: the same separation for
// the authorization limiter.
func TestTrustedProxyClientsHaveSeparateAuthorizeBuckets(t *testing.T) {
	f := proxyFixture(t)
	const proxy = "10.0.0.2:4000"
	for i := 0; i < authorizePerWindow; i++ {
		f.authorizeFrom(t, proxy, "198.51.100.7")
	}
	if code := f.authorizeFrom(t, proxy, "198.51.100.7"); code != http.StatusTooManyRequests {
		t.Fatalf("client A over budget = %d, want 429", code)
	}
	if code := f.authorizeFrom(t, proxy, "198.51.100.8"); code == http.StatusTooManyRequests {
		t.Fatal("client B shares A's authorize bucket")
	}
}

// TestUntrustedPeerCannotEscapeRegisterBucket: a peer outside the trusted list
// that rotates X-Forwarded-For still hits one bucket.
func TestUntrustedPeerCannotEscapeRegisterBucket(t *testing.T) {
	f := proxyFixture(t)
	const peer = "203.0.113.9:4000"
	for i := 0; i < registerPerWindow; i++ {
		_ = f.registerFrom(t, peer, "192.0.2."+strings.Repeat("1", 1+i%3))
	}
	if code := f.registerFrom(t, peer, "192.0.2.250"); code != http.StatusTooManyRequests {
		t.Fatalf("untrusted peer escaped its bucket via XFF: status = %d, want 429", code)
	}
}

// TestUntrustedPeerCannotEscapeAuthorizeBucket is the same check for authorize.
func TestUntrustedPeerCannotEscapeAuthorizeBucket(t *testing.T) {
	f := proxyFixture(t)
	const peer = "203.0.113.9:4000"
	for i := 0; i < authorizePerWindow; i++ {
		f.authorizeFrom(t, peer, "192.0.2.1")
	}
	if code := f.authorizeFrom(t, peer, "192.0.2.250"); code != http.StatusTooManyRequests {
		t.Fatalf("untrusted peer escaped its authorize bucket: status = %d, want 429", code)
	}
}

// TestMalformedXFFFromTrustedProxyUsesPeerBucket: a trusted proxy sending a
// broken header shares the peer's bucket, so garbage cannot mint new buckets.
func TestMalformedXFFFromTrustedProxyUsesPeerBucket(t *testing.T) {
	f := proxyFixture(t)
	const proxy = "10.0.0.2:4000"
	for i := 0; i < registerPerWindow; i++ {
		_ = f.registerFrom(t, proxy, "garbage-"+strings.Repeat("x", i))
	}
	if code := f.registerFrom(t, proxy, "198.51.100.99"); code == http.StatusTooManyRequests {
		t.Fatal("well-formed XFF escaped a bucket that malformed XFF filled")
	}
	if code := f.registerFrom(t, proxy, "garbage-final"); code != http.StatusTooManyRequests {
		t.Fatalf("malformed XFF did not share the peer bucket: status = %d, want 429", code)
	}
}
