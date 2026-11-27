package netguard

import (
	"net/netip"
	"testing"
)

func TestAllowedPublicAddr(t *testing.T) {
	for _, v := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", // loopback, RFC 1918
		"169.254.169.254", "100.64.0.1", "100.127.255.255", "0.1.2.3", "0.0.0.0", // link-local, CGNAT, 0/8
		"192.0.0.1", "192.0.2.1", "198.18.0.1", "198.19.255.255", "198.51.100.1", "203.0.113.1", // special-use, documentation, benchmark
		"240.0.0.1", "255.255.255.255", "224.0.0.1", "239.255.255.250", // reserved, broadcast, multicast
		"::1", "::", "fc00::1", "fd12:3456::1", "fe80::1", "ff02::1", // IPv6 loopback, unspecified, ULA, link-local, multicast
		"2001::1", "2001:1ff::1", "2001:db8::1", "2002:7f00:1::1", "3fff::1", // Teredo, 2001::/23, documentation, 6to4, 3fff::/20
		"64:ff9b::7f00:1", "::127.0.0.1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", // NAT64, IPv4-compatible, IPv4-mapped
		"fe80::1%eth0",
	} {
		if AllowedPublicAddr(netip.MustParseAddr(v)) {
			t.Fatalf("allowed %s", v)
		}
	}
	for _, v := range []string{"8.8.8.8", "1.1.1.1", "160.79.104.10", "100.63.255.255", "100.128.0.0", "::ffff:8.8.8.8", "2606:4700::1111", "2001:4860:4860::8888", "2400:cb00::1"} {
		if !AllowedPublicAddr(netip.MustParseAddr(v)) {
			t.Fatalf("refused %s", v)
		}
	}
}
