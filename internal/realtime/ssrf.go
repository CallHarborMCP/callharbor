package realtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
)

// ErrSSRFBlocked reports a dial refused by the endpoint policy. It never
// includes the resolved address, so it cannot be used to probe internal hosts.
var ErrSSRFBlocked = errors.New("realtime: endpoint address is not allowed")

// EndpointPolicy controls which resolved addresses a provider dial may reach.
// The zero value allows only public addresses, which is the default for every
// user-supplied endpoint.
type EndpointPolicy struct {
	// AllowPrivate permits loopback, RFC 1918, link-local and other non-public
	// addresses. It is set only by an explicit, admin-approved deployment
	// policy for self-hosted gateways.
	AllowPrivate bool
}

// blockedPrefixes are the non-public ranges refused unless AllowPrivate is set.
// Link-local covers the cloud metadata address 169.254.169.254.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// Allowed reports whether ip may be dialed under the policy.
func (p EndpointPolicy) Allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return false
	}
	if p.AllowPrivate {
		return true
	}
	for _, pre := range blockedPrefixes {
		if pre.Contains(ip) {
			return false
		}
	}
	return true
}

// control runs on every connection attempt with the address that is actually
// being connected, after DNS resolution. Checking here, not on the hostname,
// defeats DNS rebinding and multi-record answers that include a private address.
func (p EndpointPolicy) control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrSSRFBlocked
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return ErrSSRFBlocked
	}
	if !p.Allowed(ip) {
		return ErrSSRFBlocked
	}
	return nil
}

// HTTPClient returns a client for provider WebSocket upgrades. Every
// connection is checked against the policy, redirects are refused, and the
// handshake is bounded by HandshakeTimeout.
func (p EndpointPolicy) HTTPClient() *http.Client {
	d := &net.Dialer{Timeout: HandshakeTimeout, Control: p.control}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := d.DialContext(ctx, network, addr)
		if errors.Is(err, ErrSSRFBlocked) {
			return nil, ErrSSRFBlocked
		}
		return conn, err
	}
	// Proxy is left nil on purpose: an environment proxy would dial the proxy
	// address instead of the endpoint and bypass the Control check above.
	tr := &http.Transport{
		DialContext:           dial,
		TLSHandshakeTimeout:   HandshakeTimeout,
		ResponseHeaderTimeout: HandshakeTimeout,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   HandshakeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("%w: redirects are not followed", ErrSSRFBlocked)
		},
	}
}
