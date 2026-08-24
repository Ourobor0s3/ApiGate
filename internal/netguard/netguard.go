// Package netguard holds the shared address checks that keep server-side
// requests away from private networks (SSRF guards): used by the site-check
// probes and by the news upstream that a Redis secret can redirect.
package netguard

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"
)

// ErrBlocked reports a refused connection to a non-public address, returned by
// RestrictedDialContext.
var ErrBlocked = errors.New("blocked address")

// RestrictedDialContext refuses connections to loopback/private/link-local
// addresses (SSRF guard). Two layers close the DNS-rebinding window: a
// pre-dial LookupIP fast-fails obviously private targets before any socket is
// opened, and a Control hook re-validates each freshly resolved address the
// dialer is about to connect to, so an independent second resolution inside
// Dial can't slip a private endpoint through. Unresolvable names pass through.
func RestrictedDialContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if base == nil {
		base = &net.Dialer{}
	}
	d := *base // copy: the caller's dialer must not gain our Control hook
	if d.Control == nil {
		d.Control = func(_, addr string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && BlockedIP(ip) {
				return ErrBlocked
			}
			return nil
		}
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if ips, err := net.LookupIP(host); err == nil {
			for _, ip := range ips {
				if BlockedIP(ip) {
					return nil, ErrBlocked
				}
			}
		}
		return d.DialContext(ctx, network, addr)
	}
}

// BlockedIP reports whether ip is a non-public address: loopback, private,
// link-local (unicast/multicast) or unspecified. These are the ranges no
// server-side HTTP client should ever contact unless explicitly allowed.
func BlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// PrivateHost reports whether host (an IP literal or a hostname) resolves to
// at least one blocked address. It fails closed: unparseable input and DNS
// errors report true, so callers never fetch an unchecked name. The lookup is
// bounded so a slow resolver can't stall the calling poller/probe.
func PrivateHost(ctx context.Context, host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return BlockedIP(ip)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := (&net.Resolver{}).LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return true
	}
	for _, a := range ips {
		if BlockedIP(a) {
			return true
		}
	}
	return false
}
