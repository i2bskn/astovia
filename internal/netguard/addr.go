// Package netguard keeps the address check that the outbound clients share:
// the AI endpoints and the push services are reached at URLs that users
// choose, so only public addresses may be dialed. The OAuth server itself
// is authlume's mcpoauth.
package netguard

import "net/netip"

var deniedIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
}

var globalIPv6 = netip.MustParsePrefix("2000::/3")

var deniedIPv6 = []netip.Prefix{
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

// AllowedPublicAddr reports whether an outbound connection may go to a.
// It is an allow list: global unicast IPv4 outside private and special-use
// ranges, and IPv6 in 2000::/3 outside special-use ranges. Everything else
// (loopback, private, link-local, CGNAT, multicast, NAT64, ...) is refused.
// IPv4-mapped IPv6 addresses are checked as IPv4.
func AllowedPublicAddr(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if a.Is4() {
		if !a.IsGlobalUnicast() || a.IsPrivate() {
			return false
		}
		for _, p := range deniedIPv4 {
			if p.Contains(a) {
				return false
			}
		}
		return true
	}
	if !a.Is6() || !globalIPv6.Contains(a) {
		return false
	}
	for _, p := range deniedIPv6 {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
