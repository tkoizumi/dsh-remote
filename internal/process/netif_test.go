package process

import (
	"net"
	"testing"
)

// TestLANAddressesExcludesTailscaleAndLoopback is written against the real
// interfaces of the machine running the test, so it skips when there is no
// non-loopback IPv4 address to find.
func TestLANAddressesExcludesTailscaleAndLoopback(t *testing.T) {
	addresses := LANAddresses()
	for _, addr := range addresses {
		ip := net.ParseIP(addr)
		if ip == nil {
			t.Fatalf("LANAddresses returned a non-IP: %q", addr)
		}
		if ip.IsLoopback() {
			t.Fatalf("LANAddresses returned a loopback address: %s", addr)
		}
		if ip.IsLinkLocalUnicast() {
			t.Fatalf("LANAddresses returned a link-local address: %s", addr)
		}
		if cgnat.Contains(ip) {
			t.Fatalf("LANAddresses returned a Tailscale/CGNAT address: %s", addr)
		}
		if ip.To4() == nil {
			t.Fatalf("LANAddresses returned a non-IPv4 address: %s", addr)
		}
	}
	if len(addresses) == 0 {
		t.Skip("no non-loopback IPv4 address on this machine")
	}
	t.Logf("local network addresses: %v", addresses)
}
