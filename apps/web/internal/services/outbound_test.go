package services_test

import (
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/apps/web/internal/services"
)

// The fetch adapter runs PublicAddr on every IP it dials (after DNS, on
// every redirect hop), so these cases are what a rebinding or redirect
// attack can land on.
func TestPublicAddr(test *testing.T) {
	test.Parallel()
	cases := map[string]bool{
		"93.184.215.14":        true, // public IPv4
		"8.8.8.8":              true,
		"2606:4700:4700::1111": true, // public IPv6
		"::ffff:8.8.8.8":       true, // IPv4-mapped public

		"127.0.0.1":                 false, // loopback
		"127.255.255.254":           false,
		"::1":                       false,
		"::ffff:127.0.0.1":          false, // IPv4-mapped loopback
		"10.0.0.1":                  false, // RFC 1918
		"172.16.0.1":                false,
		"172.31.255.255":            false,
		"192.168.1.1":               false,
		"169.254.169.254":           false, // cloud metadata (link-local)
		"169.254.0.1":               false,
		"fe80::1":                   false, // IPv6 link-local
		"fe80::1%eth0":              false,
		"fd00:ec2::254":             false, // AWS IPv6 metadata (unique local)
		"fc00::1":                   false,
		"100.64.0.1":                false, // carrier-grade NAT / Tailscale
		"100.100.100.200":           false, // Alibaba metadata, CGNAT range
		"0.0.0.0":                   false, // unspecified / "this network"
		"0.1.2.3":                   false,
		"::":                        false,
		"224.0.0.1":                 false, // multicast
		"ff02::1":                   false,
		"255.255.255.255":           false, // broadcast
		"240.0.0.1":                 false, // reserved
		"192.0.2.1":                 false, // documentation
		"198.51.100.1":              false,
		"203.0.113.1":               false,
		"2001:db8::1":               false,
		"198.18.0.1":                false, // benchmarking
		"64:ff9b::7f00:1":           false, // NAT64 of 127.0.0.1
		"64:ff9b::a9fe:a9fe":        false, // NAT64 of 169.254.169.254
		"64:ff9b::808:808":          true,  // NAT64 of 8.8.8.8
		"2002:7f00:1::":             false, // 6to4 of 127.0.0.1
		"2002:c0a8:101::":           false, // 6to4 of 192.168.1.1
		"2002:808:808::":            true,  // 6to4 of 8.8.8.8
		"2001:0:4136:e378::":        false, // Teredo
		"::127.0.0.1":               false, // IPv4-compatible (deprecated)
		"100::1":                    false, // discard
		"64:ff9b:1::7f00:1":         false, // local-use NAT64
		"3fff::1":                   false, // documentation (RFC 9637)
		"2001:2::1":                 false, // benchmarking
		"2001:20::1":                false, // ORCHIDv2
		"2600:1f18:1234:5678::abcd": true,  // public AWS IPv6
	}
	for raw, want := range cases {
		if got := services.PublicAddr(netip.MustParseAddr(raw)); got != want {
			test.Errorf("PublicAddr(%s) = %v, want %v", raw, got, want)
		}
	}
	if services.PublicAddr(netip.Addr{}) {
		test.Error("PublicAddr(zero) = true")
	}
}

func TestFetchURLAllowed(test *testing.T) {
	test.Parallel()
	long := "https://example.com/" + strings.Repeat("a", services.MaxImageURLLength)
	cases := map[string]bool{
		"https://example.com/a.png":     true,
		"http://example.com/a.png":      true,
		"https://example.com:443/a.png": true,
		"http://example.com:80/a.png":   true,
		"https://93.184.215.14/a.png":   true, // a literal public IP is judged at dial time
		"https://[2606:4700::1]/a.png":  true,

		"ftp://example.com/a.png":           false, // scheme
		"file:///etc/passwd":                false,
		"gopher://127.0.0.1:6379/_FLUSHALL": false,
		"data:image/png;base64,AAAA":        false,
		"https://user:pw@example.com/":      false, // credentials
		"https://user@example.com/":         false,
		"https:///a.png":                    false, // no host
		"https://example.com:8080/":         false, // non-default ports (internal services)
		"http://example.com:443/":           false,
		"https://example.com:80/":           false,
		"https://example.com:6379/":         false,
		"https://[fe80::1%25eth0]/":         false, // IPv6 zone
		"http://2130706433/":                false, // 127.0.0.1 as a decimal integer
		"http://0x7f000001/":                false, // hex
		"http://0x7f.1/":                    false,
		"http://127.1/":                     false, // short form
		"http://017700000001/":              false, // octal
		"http://example.0x10/":              false,
		"http://169.254.169.254.":           false, // trailing dot, last label numeric
		long:                                false, // too long
	}
	for raw, want := range cases {
		target, err := url.Parse(raw)
		if err != nil {
			test.Fatalf("parse %q: %v", raw, err)
		}
		if err := services.FetchURLAllowed(target); (err == nil) != want {
			test.Errorf("FetchURLAllowed(%.60q) = %v, want allowed=%v", raw, err, want)
		}
	}
	if err := services.FetchURLAllowed(nil); err == nil {
		test.Error("FetchURLAllowed(nil) = nil")
	}
}
