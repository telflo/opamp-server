package middleware

import (
	"net/http"
	"strings"
	"testing"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name string
		xff  string
		xrip string
		addr string
		want string
	}{
		{"xff_leftmost", "203.0.113.5, 10.0.0.1", "", "10.0.0.1:52344", "203.0.113.5"},
		{"xff_single", "198.51.100.7", "", "10.0.0.1:52344", "198.51.100.7"},
		{"xrip_fallback", "", "198.51.100.9", "10.0.0.1:52344", "198.51.100.9"},
		{"remoteaddr_strips_port", "", "", "192.0.2.4:41234", "192.0.2.4"},
		{"remoteaddr_no_port", "", "", "192.0.2.4", "192.0.2.4"},
		{"empty_xff_falls_through", "  ", "", "192.0.2.9:9", "192.0.2.9"},
		{"remoteaddr_ipv6", "", "", "[::1]:8080", "::1"},
		{"xff_empty_leftmost_falls_through", " ,10.0.0.1", "", "192.0.2.9:9", "192.0.2.9"},
		{"xff_ipv6", "2001:db8::7, 10.0.0.1", "", "10.0.0.1:52344", "2001:db8::7"},
		// The chosen value must be an IP address; anything else is dropped,
		// never stored as the collector's address.
		{"xff_not_an_address_is_dropped", "collector-canary.example, 10.0.0.1", "", "192.0.2.9:9", ""},
		{"xff_with_a_port_is_dropped", "203.0.113.5:443", "", "192.0.2.9:9", ""},
		{"xff_free_text_is_dropped", "<script>", "", "192.0.2.9:9", ""},
		{"xrip_not_an_address_is_dropped", "", "proxy.internal", "192.0.2.9:9", ""},
		{"remoteaddr_not_an_address_is_dropped", "", "", "@pipe", ""},
		{"a_zone_is_dropped", "fe80::1%eth0", "", "192.0.2.9:9", "fe80::1"},
		{"an_ipv4_mapped_address_is_ipv4", "::ffff:192.0.2.1", "", "192.0.2.9:9", "192.0.2.1"},
		{"an_address_too_long_is_dropped", strings.Repeat("1", 300), "", "192.0.2.9:9", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest("POST", "/opamp/api/v1", nil)
			r.RemoteAddr = tc.addr
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xrip != "" {
				r.Header.Set("X-Real-IP", tc.xrip)
			}
			if got := ClientIP(r); got != tc.want {
				t.Fatalf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}
