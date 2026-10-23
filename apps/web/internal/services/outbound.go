package services

import (
	"net/netip"
	"net/url"
	"strings"
)

// Outbound fetch rules for URLs that come from users (ImageService.Import).
// The fetch adapter (packages/storage/safefetch, wired in cmd/server) runs
// PublicAddr on every IP it dials, after DNS and on every redirect hop, so
// DNS rebinding cannot slip past it, and FetchURLAllowed on the first URL and
// every redirect target. They live here so their cases are tested.

// deniedPrefixes are refused on top of the net/netip predicates (loopback,
// private, link-local, multicast, unspecified). IPv6 is already limited to
// global unicast 2000::/3, so the IPv6 entries are inside it.
var deniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT (also Tailscale)
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local, cloud metadata 169.254.169.254
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, broadcast
	netip.MustParsePrefix("2001::/23"),       // IETF assignments: Teredo 2001::/32, benchmarking, ORCHID
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("3fff::/20"),       // documentation (RFC 9637)
}

var (
	ipv6GlobalUnicast = netip.MustParsePrefix("2000::/3")
	nat64WellKnown    = netip.MustParsePrefix("64:ff9b::/96") // RFC 6052: IPv4 in the last 32 bits
	sixToFour         = netip.MustParsePrefix("2002::/16")    // RFC 3056: IPv4 in bits 16-48
)

// Byte offsets of the embedded IPv4 address inside those IPv6 forms.
const (
	nat64IPv4Offset     = 12 // the last 4 of 16 bytes
	sixToFourIPv4Offset = 2  // right after the 2002: prefix
)

// PublicAddr reports whether addr is a publicly routable unicast address an
// outbound fetch may connect to. Refused: loopback, private, carrier-grade
// NAT, link-local (cloud metadata), multicast, broadcast, documentation,
// benchmarking and reserved ranges, and all IPv6 outside 2000::/3 (unique
// local fc00::/7, link-local fe80::/10, ...). Addresses that embed an IPv4
// address are judged by it: ::ffff:a.b.c.d, NAT64 64:ff9b::a.b.c.d and 6to4.
func PublicAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		raw := addr.As16()
		switch {
		case nat64WellKnown.Contains(addr):
			return PublicAddr(netip.AddrFrom4([4]byte(raw[nat64IPv4Offset:])))
		case sixToFour.Contains(addr):
			return PublicAddr(netip.AddrFrom4([4]byte(raw[sixToFourIPv4Offset:])))
		case !ipv6GlobalUnicast.Contains(addr):
			return false
		}
	}
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() {
		return false
	}
	for _, prefix := range deniedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// FetchURLAllowed is the URL rule for user-supplied URLs: http or https on
// the default port, a host, no credentials, no IPv6 zone, and no numeric host
// that resolvers read as IPv4 but netip does not (2130706433, 0x7f.1, 127.1).
// It returns ErrImageURLNotAllowed.
func FetchURLAllowed(target *url.URL) error {
	if target == nil || (target.Scheme != "http" && target.Scheme != "https") || target.User != nil || target.Opaque != "" {
		return ErrImageURLNotAllowed
	}
	host := target.Hostname()
	if host == "" || strings.Contains(host, "%") || len(target.String()) > MaxImageURLLength {
		return ErrImageURLNotAllowed
	}
	if _, err := netip.ParseAddr(host); err != nil && endsInNumber(host) {
		return ErrImageURLNotAllowed
	}
	defaultPort := map[string]string{"http": "80", "https": "443"}[target.Scheme]
	if port := target.Port(); port != "" && port != defaultPort {
		return ErrImageURLNotAllowed
	}
	return nil
}

// endsInNumber reports whether the last label of host is a number (decimal
// or 0x hex): the WHATWG rule for "this host is an IPv4 address".
func endsInNumber(host string) bool {
	host = strings.TrimSuffix(host, ".")
	last := strings.ToLower(host[strings.LastIndexByte(host, '.')+1:])
	if hex, ok := strings.CutPrefix(last, "0x"); ok {
		return strings.Trim(hex, "0123456789abcdef") == ""
	}
	return last != "" && strings.Trim(last, "0123456789") == ""
}
