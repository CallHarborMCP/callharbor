package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	EnvListenAddr      = "CALLHARBOR_LISTEN_ADDR"
	EnvDataDir         = "CALLHARBOR_DATA_DIR"
	EnvLogLevel        = "CALLHARBOR_LOG_LEVEL"
	EnvShutdownTimeout = "CALLHARBOR_SHUTDOWN_TIMEOUT"
	// EnvPublicURL is the absolute public origin of this installation, for
	// example https://callharbor.example.com. It is the OAuth issuer and the
	// MCP resource base. When unset, OAuth and MCP routes are not mounted.
	EnvPublicURL = "CALLHARBOR_PUBLIC_URL"
	// EnvTrustedProxies lists the peers whose X-Forwarded-For header is
	// believed when rate limits pick a client address. Entries are IP addresses
	// or CIDR prefixes separated by commas. Unset trusts no proxy.
	EnvTrustedProxies = "CALLHARBOR_TRUSTED_PROXIES"
	// EnvMaxCallBriefBytes bounds the canonical JSON size of one call brief
	// (context, instructions, constraints and the rest). Oversized briefs are
	// rejected with brief_too_large, never truncated. The value is in bytes.
	EnvMaxCallBriefBytes = "CALLHARBOR_MAX_CALL_BRIEF_BYTES"

	defaultListenAddr      = ":8080"
	defaultDataDir         = "/data"
	defaultShutdownTimeout = 15 * time.Second
	maxShutdownTimeout     = 5 * time.Minute

	// DefaultMaxCallBriefBytes is the brief limit when the variable is unset.
	DefaultMaxCallBriefBytes = 32 << 10
	// MinMaxCallBriefBytes and MaxMaxCallBriefBytes bound the configurable
	// limit. The floor keeps a usable brief; the ceiling keeps one request from
	// holding an unbounded amount of memory and provider payload.
	MinMaxCallBriefBytes = 1 << 10
	MaxMaxCallBriefBytes = 256 << 10
)

type Config struct {
	ListenAddr      string
	DataDir         string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	// PublicURL is the normalized origin (scheme://host[:port], no trailing
	// slash). Empty means OAuth/MCP routes are disabled.
	PublicURL string
	// TrustedProxies are the peer prefixes whose X-Forwarded-For entries are
	// used to find the client for rate limiting. Empty trusts no peer.
	TrustedProxies []netip.Prefix
	// MaxCallBriefBytes is the largest accepted canonical call brief, in bytes.
	MaxCallBriefBytes int
}

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		ListenAddr:        value(getenv, EnvListenAddr, defaultListenAddr),
		DataDir:           value(getenv, EnvDataDir, defaultDataDir),
		LogLevel:          slog.LevelInfo,
		ShutdownTimeout:   defaultShutdownTimeout,
		MaxCallBriefBytes: DefaultMaxCallBriefBytes,
	}

	var errs []error

	if err := validateListenAddr(cfg.ListenAddr); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvListenAddr, err))
	}

	if !filepath.IsAbs(cfg.DataDir) {
		errs = append(errs, fmt.Errorf("%s: must be an absolute path", EnvDataDir))
	} else {
		cfg.DataDir = filepath.Clean(cfg.DataDir)
	}

	if raw := strings.TrimSpace(getenv(EnvLogLevel)); raw != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(raw)); err != nil {
			errs = append(errs, fmt.Errorf("%s: must be one of debug, info, warn or error", EnvLogLevel))
		}
	}

	if raw := strings.TrimSpace(getenv(EnvShutdownTimeout)); raw != "" {
		timeout, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: must be a duration such as 15s", EnvShutdownTimeout))
		case timeout <= 0 || timeout > maxShutdownTimeout:
			errs = append(errs, fmt.Errorf("%s: must be greater than 0 and at most %s", EnvShutdownTimeout, maxShutdownTimeout))
		default:
			cfg.ShutdownTimeout = timeout
		}
	}

	if raw := strings.TrimSpace(getenv(EnvPublicURL)); raw != "" {
		origin, err := parsePublicURL(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvPublicURL, err))
		} else {
			cfg.PublicURL = origin
		}
	}

	if raw := strings.TrimSpace(getenv(EnvTrustedProxies)); raw != "" {
		prefixes, err := parseTrustedProxies(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvTrustedProxies, err))
		} else {
			cfg.TrustedProxies = prefixes
		}
	}

	if raw := strings.TrimSpace(getenv(EnvMaxCallBriefBytes)); raw != "" {
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: must be a whole number of bytes", EnvMaxCallBriefBytes))
		case n < MinMaxCallBriefBytes || n > MaxMaxCallBriefBytes:
			errs = append(errs, fmt.Errorf("%s: must be between %d and %d bytes", EnvMaxCallBriefBytes, MinMaxCallBriefBytes, MaxMaxCallBriefBytes))
		default:
			cfg.MaxCallBriefBytes = n
		}
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// parseTrustedProxies parses comma-separated IP addresses and CIDR prefixes.
// Whitespace around each entry is ignored, but an empty entry is an error so a
// stray comma cannot pass silently. A zoned address is refused, because a zone
// names an interface on one host and cannot identify a peer.
func parseTrustedProxies(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, errors.New("entries must be IP addresses or CIDR prefixes separated by commas, with no empty entry")
		}
		if strings.Contains(entry, "%") {
			return nil, fmt.Errorf("zoned address %q is not allowed", entry)
		}
		p, err := parseTrustedEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("%q must be an IP address or CIDR prefix", entry)
		}
		out = append(out, p)
	}
	return out, nil
}

func parseTrustedEntry(entry string) (netip.Prefix, error) {
	if strings.Contains(entry, "/") {
		p, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, err
		}
		return normalizePrefix(p), nil
	}
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Prefix{}, err
	}
	return normalizePrefix(netip.PrefixFrom(addr, addr.BitLen())), nil
}

// normalizePrefix rewrites an IPv4-mapped IPv6 prefix in IPv4 form. Peers are
// compared unmapped, so ::ffff:10.0.0.0/104 must become 10.0.0.0/8 to match.
func normalizePrefix(p netip.Prefix) netip.Prefix {
	p = p.Masked()
	if a := p.Addr(); a.Is4In6() && p.Bits() >= 96 {
		return netip.PrefixFrom(a.Unmap(), p.Bits()-96).Masked()
	}
	return p
}

// parsePublicURL accepts an absolute origin. https is required except for
// loopback hosts, which may use http for local development. Paths, queries,
// fragments, userinfo and empty hosts are rejected. A trailing slash is
// normalized away.
func parsePublicURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("must be an absolute origin such as https://callharbor.example.com")
	}
	if u.Opaque != "" || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", errors.New("must be an origin with no userinfo, query or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("must not contain a path")
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New("must include a host")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(host) {
			return "", errors.New("http is allowed only for loopback hosts; use https")
		}
	default:
		return "", errors.New("scheme must be https (or http for loopback hosts)")
	}
	// The origin is normalized to one spelling: a lowercase host and no port
	// when it is the scheme's default. Metadata and issuer checks compare it.
	port := u.Port()
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", errors.New("port must be between 1 and 65535")
		}
	}
	origin := u.Scheme + "://" + strings.ToLower(host)
	if strings.Contains(host, ":") {
		origin = u.Scheme + "://[" + strings.ToLower(host) + "]" // IPv6 literal keeps its brackets
	}
	if port != "" && !isDefaultPort(u.Scheme, port) {
		origin += ":" + port
	}
	return origin, nil
}

// isDefaultPort reports whether port is the default for scheme, which the
// origin omits.
func isDefaultPort(scheme, port string) bool {
	return (scheme == "https" && port == "443") || (scheme == "http" && port == "80")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

func value(getenv func(string) string, key, fallback string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return fallback
}
