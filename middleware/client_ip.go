package middleware

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP returns the collector's originating IP for display/diagnostics.
//
// Behind a load balancer or reverse proxy, r.RemoteAddr is the proxy, so the
// real client is in the forwarded headers. Preference order:
//
//  1. X-Forwarded-For, leftmost entry — each proxy appends the address it
//     received from, so the leftmost hop is the originating collector.
//  2. X-Real-IP — single-value fallback some proxies set.
//  3. RemoteAddr (host portion) — direct connections in local/dev with no
//     proxy in front.
//
// X-Forwarded-For is client-settable and therefore spoofable; that is
// acceptable because the value is only ever displayed, never trusted.
//
// The value chosen must be an IP address (netip.ParseAddr), written in its
// canonical form without a zone, an IPv4-mapped IPv6 address as IPv4; any
// other text (a name, an address with a port, anything a client made up) is
// dropped and the answer is "", so what the poll stores as the collector's
// reported address is always an address that fits it and never free text
// from a header.
func ClientIP(r *http.Request) string {
	chosen := ""
	switch {
	case firstForwarded(r) != "":
		chosen = firstForwarded(r)
	case strings.TrimSpace(r.Header.Get("X-Real-IP")) != "":
		chosen = strings.TrimSpace(r.Header.Get("X-Real-IP"))
	default:
		chosen = strings.TrimSpace(r.RemoteAddr)
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			chosen = host
		}
	}
	addr, err := netip.ParseAddr(chosen)
	if err != nil {
		return ""
	}
	return addr.WithZone("").Unmap().String()
}

// firstForwarded is X-Forwarded-For's leftmost entry, trimmed, or "".
func firstForwarded(r *http.Request) string {
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return ""
	}
	first, _, _ := strings.Cut(xff, ",")
	return strings.TrimSpace(first)
}
