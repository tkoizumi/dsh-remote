package process

import (
	"net"
	"reflect"
	"testing"
)

// up builds an up interface snapshot for the selection tests.
func up(name string, ips ...string) ifaceSnapshot {
	snap := ifaceSnapshot{Name: name, Flags: net.FlagUp}
	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			panic("bad test IP: " + ip)
		}
		snap.IPs = append(snap.IPs, parsed)
	}
	return snap
}

// TestSelectPrefersDefaultRouteOverDockerBridge is the regression test for the
// reported defect: a Docker bridge (172.17.0.1) sorts before the real LAN
// address (192.168.0.151), so the old lexical selection advertised the bridge.
// The default route must decide, not the lexical order.
func TestSelectPrefersDefaultRouteOverDockerBridge(t *testing.T) {
	ifaces := []ifaceSnapshot{
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback, IPs: []net.IP{net.ParseIP("127.0.0.1")}},
		up("docker0", "172.17.0.1"),
		up("enp3s0", "192.168.0.151"),
		up("tailscale0", "100.96.165.94"),
	}

	got := selectLANAddresses(ifaces, "enp3s0", net.ParseIP("192.168.0.151"))
	want := []string{"192.168.0.151"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectLANAddresses = %v, want %v (the Docker bridge must not win)", got, want)
	}
}

// TestSelectPrefersDefaultRouteAmongPhysicalNICs covers a host with two real
// NICs where the default route picks the right one rather than the first name.
func TestSelectPrefersDefaultRouteAmongPhysicalNICs(t *testing.T) {
	ifaces := []ifaceSnapshot{
		up("enp3s0", "192.168.1.9"),
		up("enp4s0", "10.0.0.5"),
	}

	got := selectLANAddresses(ifaces, "enp4s0", net.ParseIP("10.0.0.5"))
	want := []string{"10.0.0.5", "192.168.1.9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectLANAddresses = %v, want %v", got, want)
	}
}

// TestSelectExcludesVirtualInterfaces proves the virtual set is filtered while a
// real Wi-Fi interface is kept.
func TestSelectExcludesVirtualInterfaces(t *testing.T) {
	ifaces := []ifaceSnapshot{
		up("wlp2s0", "192.168.0.50"),
		up("docker0", "172.17.0.1"),
		up("veth12ab", "10.1.1.5"),
		up("br-a1b2c3d4e5f6", "172.18.0.1"),
		up("virbr0", "192.168.122.1"),
		up("tun0", "10.8.0.1"),
		up("wg0", "10.9.0.1"),
		up("vboxnet0", "192.168.56.1"),
		up("vEthernet (WSL)", "172.20.0.1"),
		up("tailscale0", "100.96.165.94"),
	}

	got := selectLANAddresses(ifaces, "", nil)
	want := []string{"192.168.0.50"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectLANAddresses = %v, want %v", got, want)
	}
}

// TestSelectKeepsLegitimateLANInterfaces guards the other direction: Wi-Fi,
// VLANs, bonds, and LAN bridges must never be mistaken for virtual interfaces.
func TestSelectKeepsLegitimateLANInterfaces(t *testing.T) {
	ifaces := []ifaceSnapshot{
		up("wlp2s0", "192.168.0.50"),
		up("bond0", "10.0.0.2"),
		up("eth0.100", "10.0.100.2"),
		up("br0", "10.0.0.3"),
		up("vmbr0", "10.0.0.4"),
		up("vlan10", "10.0.0.6"),
	}

	got := selectLANAddresses(ifaces, "", nil)
	for _, want := range []string{"192.168.0.50", "10.0.0.2", "10.0.100.2", "10.0.0.3", "10.0.0.4", "10.0.0.6"} {
		if !contains(got, want) {
			t.Errorf("selectLANAddresses dropped legitimate address %s: %v", want, got)
		}
	}
	if len(got) != 6 {
		t.Errorf("selectLANAddresses = %v, want all 6 legitimate addresses", got)
	}
}

// TestSelectSkipsUnusableAddresses covers loopback, link-local, CGNAT, and
// unspecified addresses, plus IPv6, none of which belong in a LAN URL.
func TestSelectSkipsUnusableAddresses(t *testing.T) {
	ifaces := []ifaceSnapshot{
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback, IPs: []net.IP{net.ParseIP("127.0.0.1")}},
		up("eth0", "169.254.10.10", "100.64.0.5", "0.0.0.0", "224.0.0.1", "192.168.1.2"),
		up("eth1", "fe80::1", "fd00::1"),
	}

	got := selectLANAddresses(ifaces, "eth0", nil)
	want := []string{"192.168.1.2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectLANAddresses = %v, want %v", got, want)
	}
}

// TestSelectWithoutDefaultRouteFallsBackToNames proves selection is still
// deterministic and sensible when there is no route to consult.
func TestSelectWithoutDefaultRouteFallsBackToNames(t *testing.T) {
	ifaces := []ifaceSnapshot{
		up("br0", "10.20.0.1"),
		up("enp3s0", "10.10.0.1"),
	}

	got := selectLANAddresses(ifaces, "", nil)
	// Both are considered physical; the numeric tie-break makes it stable.
	want := []string{"10.10.0.1", "10.20.0.1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selectLANAddresses = %v, want %v", got, want)
	}
}

// TestSelectDeduplicatesSharedAddresses keeps aliases on two interfaces from
// producing the same URL twice.
func TestSelectDeduplicatesSharedAddresses(t *testing.T) {
	ifaces := []ifaceSnapshot{
		up("eth0", "192.168.0.10"),
		up("eth0:1", "192.168.0.10"),
	}
	got := selectLANAddresses(ifaces, "", nil)
	if !reflect.DeepEqual(got, []string{"192.168.0.10"}) {
		t.Fatalf("selectLANAddresses = %v, want the address once", got)
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// TestValidateLANAddress rejects overrides that cannot reach the LAN while
// leaving real addresses and hostnames for the caller (and ListenOn) to use.
func TestValidateLANAddress(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "0.0.0.0", "224.0.0.1"} {
		if err := ValidateLANAddress(addr); err == nil {
			t.Errorf("ValidateLANAddress(%q) = nil, want an error", addr)
		}
	}
	for _, addr := range []string{"192.168.0.151", "10.0.0.5", "100.64.0.1", "lan.example", ""} {
		if err := ValidateLANAddress(addr); err != nil {
			t.Errorf("ValidateLANAddress(%q) = %v, want nil", addr, err)
		}
	}
}

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

// TestLANAddressesExcludesRealVirtualInterfaces checks the live machine: no
// address on a recognised virtual interface (a Docker bridge, a veth, or a
// tunnel) may be advertised as a LAN endpoint.
func TestLANAddressesExcludesRealVirtualInterfaces(t *testing.T) {
	ifaces := systemInterfaces()
	if len(ifaces) == 0 {
		t.Skip("no interfaces to inspect")
	}
	virtual := make(map[string]bool)
	for _, iface := range ifaces {
		if !isVirtualInterface(iface.Name) {
			continue
		}
		for _, ip := range iface.IPs {
			if v4 := ip.To4(); v4 != nil {
				virtual[v4.String()] = true
			}
		}
	}
	if len(virtual) == 0 {
		t.Skip("no IPv4 address on a virtual interface on this machine")
	}
	for _, addr := range LANAddresses() {
		if virtual[addr] {
			t.Errorf("LANAddresses advertised %s, which is on a virtual interface", addr)
		}
	}
}

// TestLANAddressesPrefersDefaultRouteOnThisHost is the end-to-end regression for
// the reported machine: when the default route resolves to a usable address,
// that address must be first. It skips on hosts whose default route is virtual
// or absent, where the fallback ordering is the correct answer instead.
func TestLANAddressesPrefersDefaultRouteOnThisHost(t *testing.T) {
	ifaces := systemInterfaces()
	route := defaultRoute(ifaces)
	if route.ip == nil || route.iface == "" {
		t.Skip("no default route with a resolvable interface on this machine")
	}
	if isVirtualInterface(route.iface) || !usableLANIPv4(route.ip) {
		t.Skip("default route is virtual or unusable; fallback ordering applies")
	}
	addresses := LANAddresses()
	if len(addresses) == 0 {
		t.Skip("no LAN addresses on this machine")
	}
	if addresses[0] != route.ip.String() {
		t.Fatalf("LANAddresses()[0] = %s, want the default route address %s (interfaces=%v)", addresses[0], route.ip, addresses)
	}
}

// TestIsLANAddressOnThisHost is the live check behind the status report's second
// opinion: advertised addresses classify as LAN addresses, and addresses on
// virtual interfaces (a real Docker bridge, when present) never do.
func TestIsLANAddressOnThisHost(t *testing.T) {
	addresses := LANAddresses()
	if len(addresses) == 0 {
		t.Skip("no LAN addresses on this machine")
	}
	for _, addr := range addresses {
		if !IsLANAddress(addr) {
			t.Errorf("IsLANAddress(%s) = false for an advertised LAN address", addr)
		}
	}

	checkedVirtual := false
	for _, iface := range systemInterfaces() {
		if !isVirtualInterface(iface.Name) {
			continue
		}
		for _, ip := range iface.IPs {
			v4 := ip.To4()
			if v4 == nil || !usableLANIPv4(v4) {
				continue
			}
			checkedVirtual = true
			if IsLANAddress(v4.String()) {
				t.Errorf("IsLANAddress(%s) = true for virtual interface %s", v4, iface.Name)
			}
		}
	}
	if !checkedVirtual {
		t.Skip("no IPv4 address on a virtual interface on this machine")
	}
}
