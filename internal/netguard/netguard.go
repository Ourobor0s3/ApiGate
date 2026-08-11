// Package netguard holds the shared address checks that keep server-side
// requests away from private networks (SSRF guards): used by the site-check
// probes and by the news upstream that a Redis secret can redirect.
package netguard

import (
	"context"
	"net"
	"time"
)

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
