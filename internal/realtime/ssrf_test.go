package realtime

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEndpointPolicyClassifiesAddresses(t *testing.T) {
	def := EndpointPolicy{}
	allowPriv := EndpointPolicy{AllowPrivate: true}

	blocked := []string{
		"127.0.0.1", "127.8.8.8",
		"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254",
		"100.64.0.1",
		"0.0.0.0",
		"224.0.0.1", "240.0.0.1",
		"::1", "::", "fc00::1", "fd12::1", "fe80::1", "ff02::1",
		"::ffff:10.0.0.1", "::ffff:169.254.169.254", "::ffff:127.0.0.1",
		// NAT64, 6to4 and Teredo addresses can embed a private IPv4 address.
		"64:ff9b::a9fe:a9fe", "64:ff9b::7f00:1", "64:ff9b:1::1",
		"2002:0a00:0001::1", "2002:7f00:0001::1",
		"2001:0:4136:e378:8000:63bf:f5ff:fffe",
	}
	for _, s := range blocked {
		ip := netip.MustParseAddr(s)
		if def.Allowed(ip) {
			t.Errorf("default policy must block %s", s)
		}
		if !allowPriv.Allowed(ip) {
			t.Errorf("AllowPrivate must permit %s", s)
		}
	}

	public := []string{"1.1.1.1", "8.8.8.8", "13.107.42.14", "2606:4700:4700::1111", "::ffff:8.8.8.8"}
	for _, s := range public {
		if !def.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("default policy must allow public %s", s)
		}
	}

	if def.Allowed(netip.Addr{}) {
		t.Error("invalid address must be refused")
	}
}

// TestSSRFPolicyBlocksPrivateDialAtConnectTime proves the check runs on the
// address actually dialed. A loopback server stands in for an internal host.
// The default policy must refuse it before any request reaches the server.
func TestSSRFPolicyBlocksPrivateDialAtConnectTime(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := EndpointPolicy{}.HTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("default policy dialed a loopback address")
	}
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("want ErrSSRFBlocked, got %v", err)
	}
	if hits != 0 {
		t.Fatalf("blocked dial still reached the server %d times", hits)
	}
}

// TestSSRFPolicyAllowsPrivateOnlyWhenApproved is the positive control: the
// same loopback server is reachable once an admin-approved policy allows it.
func TestSSRFPolicyAllowsPrivateOnlyWhenApproved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := EndpointPolicy{AllowPrivate: true}.HTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("approved policy must dial loopback: %v", err)
	}
	_ = resp.Body.Close()
}

// TestSSRFPolicyRefusesRedirects checks that a redirect cannot move the dial to
// an address the policy would not have allowed as the first hop.
func TestSSRFPolicyRefusesRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := EndpointPolicy{AllowPrivate: true}.HTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, redirector.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("redirect was followed")
	}
	if !strings.Contains(err.Error(), "redirects are not followed") {
		t.Fatalf("want redirect refusal, got %v", err)
	}
}

// TestSSRFErrorDoesNotLeakAddress checks that a blocked dial does not expose
// the resolved address it refused. The caller's own URL is echoed by net/http,
// so the host is a name that resolves to loopback; the resolved IP must not
// appear in the error.
func TestSSRFErrorDoesNotLeakAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := EndpointPolicy{}.HTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:"+strconv.Itoa(port)+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("loopback was reachable under the default policy")
	}
	if !errors.Is(err, ErrSSRFBlocked) {
		t.Fatalf("want ErrSSRFBlocked, got %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "::1") {
		t.Fatalf("error leaks the resolved address: %v", err)
	}
}
