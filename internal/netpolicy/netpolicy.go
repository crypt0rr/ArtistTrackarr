// Package netpolicy holds the address-space policy shared by every outbound
// fetcher in this service.
//
// It exists because the list below used to be written out twice - once for
// webhook targets and once for artwork origins - with a comment on each copy
// asking the reader to keep them aligned. Prose is not a gate: two copies that
// must never diverge are one copy, and the alignment is now a fact of the
// build rather than a promise in a comment.
package netpolicy

import "net"

// reserved is the address space Go classifies as global unicast but which is
// not a usable public destination. Parsed once at init: the list is a
// compile-time constant, so a malformed entry is a build-time bug and not
// something to skip silently at call time.
var reserved = mustParseCIDRs(
	"0.0.0.0/8",       // this-network/reserved addresses
	"100.64.0.0/10",   // RFC 6598 shared address space
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"240.0.0.0/4",     // reserved/future use
	"2001:db8::/32",   // IPv6 documentation
	"2001::/32",       // Teredo transition addresses
	"2002::/16",       // 6to4 transition addresses
	"64:ff9b::/96",    // well-known NAT64 prefix
	"64:ff9b:1::/48",  // network-specific NAT64 prefix
)

func mustParseCIDRs(values ...string) []*net.IPNet {
	networks := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			panic("netpolicy: malformed CIDR " + value + ": " + err.Error())
		}
		networks = append(networks, network)
	}
	return networks
}

// IsReserved reports whether ip falls in address space that is routable in
// principle but is never a legitimate upstream: shared, documentation,
// benchmarking, reserved, and the IPv6 transition prefixes that can carry an
// embedded private IPv4 destination.
//
// A nil ip is reserved. Callers reach this after their own loopback/private
// checks, and treating "no address" as usable would be the wrong default at
// the end of a deny chain.
func IsReserved(ip net.IP) bool {
	if ip == nil {
		return true
	}
	for _, network := range reserved {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
