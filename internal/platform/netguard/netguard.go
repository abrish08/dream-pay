// Package netguard stops server-side request forgery (SSRF): outgoing calls
// to merchant-supplied URLs (webhooks) must never reach this server, the
// private network or cloud metadata services.
//
// Two layers:
//   - CheckHost: refuses literal private addresses and internal names when a
//     URL is registered (a clear error for the merchant);
//   - Dialer: checks the address actually connected to, after DNS, so a name
//     that later resolves to 10.x / 127.x / 169.254.169.254 is still refused
//     (DNS rebinding).
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

var ErrBlocked = errors.New("destination address is not allowed")

var blocked = func() []netip.Prefix {
	var out []netip.Prefix
	for _, c := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
		"::/128", "::1/128", "64:ff9b::/96", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}()

// Allowed reports whether an IP is a public, routable address.
func Allowed(ip netip.Addr) bool {
	ip = ip.Unmap() // ::ffff:10.0.0.1 is 10.0.0.1
	if !ip.IsValid() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// CheckHost validates a URL host (name or literal IP) without resolving it.
func CheckHost(host string) error {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if ip, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		if !Allowed(ip) {
			return ErrBlocked
		}
		return nil
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") ||
		strings.HasSuffix(h, ".internal") || !strings.Contains(h, ".") {
		return ErrBlocked // internal names: metadata.google.internal, single-label hosts...
	}
	return nil
}

// Dialer returns a dial function that refuses non-public destinations.
func Dialer(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("netguard: %w", err)
			}
			if !Allowed(ap.Addr()) {
				return fmt.Errorf("%w: %s", ErrBlocked, ap.Addr())
			}
			return nil
		}}
	return d.DialContext
}
