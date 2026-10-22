// Package netguard stops user-supplied URLs from being used to reach internal
// services (server-side request forgery).
//
// A user can point an OpenAI-compatible provider at any base URL, and the server then
// makes requests to it. Left unchecked that turns the server into a proxy into its own
// network: the cloud metadata service, other containers, a database admin port.
//
// Two layers defend against it. ValidateBaseURL gives early, readable feedback when a
// key is saved. Transport is the real control: it refuses to connect to a non-public
// address, checking the address actually being dialled, so it also stops DNS names that
// resolve to private addresses, DNS rebinding and redirects to internal hosts.
//
// Self-hosters who deliberately run a model on their own network opt in with
// ALLOW_PRIVATE_PROVIDER_URLS=true.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned when a connection to a non-public address is refused.
var ErrBlockedAddress = errors.New("netguard: connections to private or internal addresses are not allowed")

// AllowPrivateFromEnv reports whether the operator has opted in to private provider URLs.
func AllowPrivateFromEnv() bool {
	return strings.EqualFold(os.Getenv("ALLOW_PRIVATE_PROVIDER_URLS"), "true")
}

// extraBlocked are ranges that are not caught by the standard "is this private" tests.
// The IPv6 entries are transition mechanisms that embed an IPv4 address or route to one
// (NAT64, 6to4, Teredo): reaching 64:ff9b::7f00:1 through a NAT64 gateway is reaching
// 127.0.0.1. No legitimate LLM provider is addressed that way, so they are refused
// outright rather than unwrapped.
var extraBlocked = mustPrefixes(
	"0.0.0.0/8",      // "this network"
	"100.64.0.0/10",  // carrier-grade NAT, used by some clouds for internal traffic
	"192.0.0.0/24",   // IETF protocol assignments
	"192.88.99.0/24", // deprecated 6to4 relay anycast
	"198.18.0.0/15",  // benchmarking
	"240.0.0.0/4",    // reserved
	"64:ff9b::/96",   // NAT64
	"64:ff9b:1::/48", // local-use NAT64
	"2002::/16",      // 6to4
	"2001::/32",      // Teredo
	"2001:db8::/32",  // documentation
	"100::/64",       // discard-only
	"fec0::/10",      // deprecated site-local
)

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}

// blocked reports whether ip is anything other than a public unicast address.
func blocked(ip netip.Addr) bool {
	ip = ip.Unmap() // ::ffff:10.0.0.1 is the private address 10.0.0.1
	if !ip.IsGlobalUnicast() {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, p := range extraBlocked {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

var internalSuffixes = []string{".localhost", ".internal", ".local", ".lan", ".home.arpa"}

// ValidateBaseURL checks a provider base URL. Unless allowPrivate is set it must be
// https and must not name an internal host. The check is advisory; Transport enforces it.
func ValidateBaseURL(raw string, allowPrivate bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, errors.New("base_url must be a full URL such as https://api.example.com/v1")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && allowPrivate:
	default:
		return nil, errors.New("base_url must use https")
	}
	if u.User != nil {
		return nil, errors.New("base_url must not contain credentials")
	}
	if allowPrivate {
		return u, nil
	}

	host := strings.ToLower(u.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil {
		if blocked(ip) {
			return nil, errors.New("base_url must not point at a private or internal address")
		}
		return u, nil
	}
	if host == "localhost" {
		return nil, errors.New("base_url must not point at a private or internal address")
	}
	for _, s := range internalSuffixes {
		if strings.HasSuffix(host, s) {
			return nil, errors.New("base_url must not point at a private or internal address")
		}
	}
	return u, nil
}

// Transport returns an HTTP transport that refuses to connect to non-public addresses
// unless allowPrivate is set. It never uses a proxy from the environment, because the
// guard would then check the proxy's address instead of the destination.
func Transport(allowPrivate bool) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrBlockedAddress, err)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || blocked(ip) {
				return ErrBlockedAddress
			}
			return nil
		}
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		MaxIdleConns:          20,
		IdleConnTimeout:       60 * time.Second,
	}
}
