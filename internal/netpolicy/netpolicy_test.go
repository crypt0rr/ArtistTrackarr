package netpolicy

import (
	"net"
	"testing"
)

func TestReservedAddressSpaceIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		ip   string
		want bool
	}{
		{"this-network", "0.1.2.3", true},
		{"carrier-grade NAT", "100.64.0.1", true},
		{"IETF protocol assignments", "192.0.0.1", true},
		{"TEST-NET-1", "192.0.2.5", true},
		{"benchmarking", "198.19.0.1", true},
		{"TEST-NET-2", "198.51.100.7", true},
		{"TEST-NET-3", "203.0.113.9", true},
		{"reserved future use", "250.1.1.1", true},
		{"IPv6 documentation", "2001:db8::1", true},
		{"Teredo", "2001:0:1::1", true},
		{"6to4", "2002::1", true},
		{"well-known NAT64", "64:ff9b::7f00:1", true},
		{"network-specific NAT64", "64:ff9b:1::1", true},

		// Ordinary public addresses must stay reachable: a policy that
		// rejects everything would pass every "is it blocked" assertion
		// above while breaking the service outright.
		{"public IPv4", "93.184.216.34", false},
		{"public IPv6", "2606:2800:220:1:248:1893:25c8:1946", false},
		{"Cloudflare resolver", "1.1.1.1", false},

		// Not this package's job - callers check these before reaching here -
		// so they must come back false, or a caller could quietly start
		// relying on the wrong layer for them.
		{"loopback is the caller's check", "127.0.0.1", false},
		{"RFC 1918 is the caller's check", "10.0.0.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("test fixture %q is not a valid IP", tc.ip)
			}
			if got := IsReserved(ip); got != tc.want {
				t.Errorf("IsReserved(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

// The deny chain ends here, so an absent address must not read as usable.
func TestANilAddressIsReserved(t *testing.T) {
	if !IsReserved(nil) {
		t.Error("IsReserved(nil) = false, want true: nil must not pass a deny chain")
	}
}

// The list is a compile-time constant. A malformed entry should stop the
// build rather than silently drop a network from the policy, which is how
// the previous per-call net.ParseCIDR behaved.
func TestAMalformedCIDRPanicsRatherThanSilentlyDroppingIt(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("mustParseCIDRs accepted a malformed CIDR, want panic")
		}
	}()
	mustParseCIDRs("192.0.2.0/33")
}
