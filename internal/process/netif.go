package process

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
)

// cgnat is the carrier-grade NAT range. Tailscale hands out addresses from it,
// and Tailscale already has its own path onto this host, so those addresses are
// not what "the local network" means.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// defaultRouteScore is the bonus an address receives when it belongs to the
// interface the host's default route uses. It dominates every name-based score
// so the address other machines can actually reach wins over a lexically
// smaller one, which is how a Docker bridge (172.17.0.1) used to beat the real
// LAN address (192.168.0.151).
const defaultRouteScore = 100

// physicalScore is the bonus for an interface whose name follows an ordinary
// physical, Wi-Fi, VLAN, bonded, or bridge convention.
const physicalScore = 10

// virtualPrefixes name interfaces that are virtual by construction and must
// never be advertised as a local network endpoint: a browser on another LAN
// machine cannot reach the Docker bridge (172.17.0.1), a container veth, or a
// tunnel address. Prefixes are matched case-insensitively against the whole
// start of the interface name.
//
// Deliberately absent: bond*, team*, br0, vmbr*, bridge*, wlan*, and vlan
// sub-interfaces. Those are legitimate LAN interfaces, and excluding them would
// break the very hosts this list is meant to protect. Docker's *custom* bridges
// are "br-<id>", which is why the "br-" prefix is here while plain "br" is not.
var virtualPrefixes = []string{
	"docker",    // Docker's default bridge
	"br-",       // Docker's per-network bridges (br-<12 hex>)
	"veth",      // container side of a veth pair
	"virbr",     // libvirt
	"vnet",      // libvirt VM tap
	"vmnet",     // VMware
	"vboxnet",   // VirtualBox host-only
	"lxc",       // LXC
	"cni",       // Kubernetes CNI
	"flannel",   // Kubernetes
	"cali",      // Calico
	"kube",      // Kubernetes
	"tun",       // OpenVPN and other tun devices
	"tap",       // tap devices
	"utun",      // macOS tunnels
	"wg",        // WireGuard
	"zt",        // ZeroTier
	"tailscale", // Tailscale (also excluded by its CGNAT address)
	"nordlynx",  // NordVPN
	"proton",    // ProtonVPN
	"ppp",       // point-to-point links
	"rmnet",     // mobile broadband
	"ham",       // Hamachi
	"dummy",     // kernel dummy
	"ifb",       // intermediate functional block
	"nlmon",     // netlink monitor
	"stf",       // 6to4
	"sit",       // 6in4
	"ip6tnl",    // IPv6 tunnel
	"ip6gre",    // IPv6 GRE
	"gre",       // GRE
	"gretap",    // GRE tap
	"erspan",    // ERSPAN
	"vxlan",     // VXLAN
	"geneve",    // Geneve
	"awdl",      // macOS Apple Wireless Direct Link
	"llw",       // macOS low-latency WLAN
	"anpi",      // macOS Apple network proxy interface
}

// physicalPrefixes name interfaces that conventionally hold an address machines
// on the LAN can reach. They only order otherwise-equal candidates; a name that
// matches nothing is still included, so unusual or platform-specific names are
// never silently dropped.
var physicalPrefixes = []string{
	"en",     // enp3s0, ens3, eno1, en0 (macOS)
	"eth",    // eth0
	"em",     // em0 (Solaris/BSD style)
	"ib",     // InfiniBand
	"wl",     // wlp2s0, wlan0
	"wwan",   // WWAN
	"ath",    // ath0
	"ra",     // ra0 (Ralink Wi-Fi)
	"bond",   // bond0, bond0.100
	"team",   // team0
	"br",     // br0, bridge0, and vmbr0 also matches below
	"vmbr",   // Proxmox LAN bridge
	"bridge", // macOS bridge0
	"vlan",   // vlan10
}

// ifaceSnapshot is the per-interface information LAN selection needs. Keeping
// it separate from net.Interface is what makes the selection testable on a
// machine that has only one real interface.
type ifaceSnapshot struct {
	Name  string
	Flags net.Flags
	IPs   []net.IP
}

// routeInfo names the interface and source address of the host's default route.
type routeInfo struct {
	iface string
	ip    net.IP
}

// LANAddresses returns this host's non-loopback IPv4 addresses that are
// plausible endpoints on the local network, most preferred first.
//
// Preference is the address of the interface the default route uses, then
// ordinary physical, Wi-Fi, VLAN, bonded, and bridged interfaces. Loopback,
// link-local, Tailscale/CGNAT, Docker bridges, container veths, and tunnel/VPN
// interfaces are excluded, because advertising one of those produces a URL that
// no other machine on the LAN can open.
func LANAddresses() []string {
	ifaces := systemInterfaces()
	route := defaultRoute(ifaces)
	return selectLANAddresses(ifaces, route.iface, route.ip)
}

// ValidateLANAddress rejects an explicit --lan-address that cannot name a LAN
// endpoint. A hostname is left for ListenOn to resolve, and an address the user
// deliberately chose is otherwise preserved.
func ValidateLANAddress(addr string) error {
	ip := net.ParseIP(strings.Trim(addr, "[]"))
	if ip == nil {
		return nil
	}
	v4 := ip.To4()
	if v4 == nil {
		return fmt.Errorf("--lan-address %s is not an IPv4 address; the local network listener is IPv4 only", addr)
	}
	switch {
	case v4.IsLoopback():
		return fmt.Errorf("--lan-address %s is a loopback address; it would not be reachable from the local network", addr)
	case v4.IsUnspecified():
		return fmt.Errorf("--lan-address %s is not a specific interface address", addr)
	case v4.IsMulticast():
		return fmt.Errorf("--lan-address %s is a multicast address, not an interface address", addr)
	}
	return nil
}

// IsLANAddress reports whether addr currently sits on an up, non-virtual
// interface that is usable as a local network endpoint.
//
// It is a second opinion on an address recorded earlier: an address can answer
// locally and still be unreachable from other machines. That is exactly the
// reported defect -- 172.17.0.1 on the Docker bridge accepts connections from
// this host while being invisible to the rest of the LAN.
func IsLANAddress(addr string) bool {
	ip := net.ParseIP(strings.Trim(addr, "[]"))
	if ip == nil {
		return false
	}
	v4 := ip.To4()
	if v4 == nil || !usableLANIPv4(v4) {
		return false
	}
	for _, iface := range systemInterfaces() {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || isVirtualInterface(iface.Name) {
			continue
		}
		for _, candidate := range iface.IPs {
			if candidate.To4() != nil && candidate.Equal(v4) {
				return true
			}
		}
	}
	return false
}

// selectLANAddresses is the whole selection policy as a pure function, so it can
// be exercised against synthetic multi-interface hosts. preferredIface and
// preferredIP come from the default route and may be zero when there is none.
func selectLANAddresses(ifaces []ifaceSnapshot, preferredIface string, preferredIP net.IP) []string {
	var candidates []lanCandidate
	seen := make(map[string]bool)

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualInterface(iface.Name) {
			continue
		}
		for _, ip := range iface.IPs {
			v4 := ip.To4()
			if v4 == nil || !usableLANIPv4(v4) {
				continue
			}
			key := v4.String()
			if seen[key] {
				continue
			}
			seen[key] = true

			score := interfaceScore(iface.Name)
			if preferredIP != nil && preferredIP.Equal(v4) {
				score += defaultRouteScore
			} else if preferredIface != "" && iface.Name == preferredIface {
				score += defaultRouteScore
			}
			candidates = append(candidates, lanCandidate{IP: v4, Score: score})
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		// Deterministic tie-break so repeated runs and tests agree.
		return bytes.Compare(candidates[i].IP, candidates[j].IP) < 0
	})

	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.IP.String())
	}
	return out
}

// lanCandidate is one usable IPv4 address and its preference score.
type lanCandidate struct {
	IP    net.IP
	Score int
}

// usableLANIPv4 reports whether an address can be advertised as a LAN endpoint.
func usableLANIPv4(ip net.IP) bool {
	return ip != nil &&
		!ip.IsLoopback() &&
		!ip.IsUnspecified() &&
		!ip.IsMulticast() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!cgnat.Contains(ip)
}

// isVirtualInterface reports whether a name belongs to an interface that only
// exists inside the host (or a tunnel), not on the local network.
func isVirtualInterface(name string) bool {
	n := strings.ToLower(name)
	for _, prefix := range virtualPrefixes {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	// Windows names its Hyper-V and WSL adapters "vEthernet (...)" and
	// "vEthernet (WSL (...))"; neither is a LAN endpoint.
	return strings.Contains(n, "vethernet") || strings.Contains(n, "wsl")
}

// interfaceScore prefers interfaces that conventionally reach the LAN.
func interfaceScore(name string) int {
	n := strings.ToLower(name)
	for _, prefix := range physicalPrefixes {
		if strings.HasPrefix(n, prefix) {
			return physicalScore
		}
	}
	return 0
}

// systemInterfaces snapshots the live interface list.
func systemInterfaces() []ifaceSnapshot {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]ifaceSnapshot, 0, len(ifaces))
	for _, iface := range ifaces {
		snap := ifaceSnapshot{Name: iface.Name, Flags: iface.Flags}
		addrs, err := iface.Addrs()
		if err != nil {
			out = append(out, snap)
			continue
		}
		for _, addr := range addrs {
			switch v := addr.(type) {
			case *net.IPNet:
				snap.IPs = append(snap.IPs, v.IP)
			case *net.IPAddr:
				snap.IPs = append(snap.IPs, v.IP)
			}
		}
		out = append(out, snap)
	}
	return out
}

// defaultRoute identifies the interface and source address the kernel uses for
// traffic that is not on a directly connected network. An empty result means the
// host has no default route, in which case selection falls back to name order.
func defaultRoute(ifaces []ifaceSnapshot) routeInfo {
	if ip := udpRouteSource(); ip != nil {
		return routeInfo{iface: ifaceNameForIP(ifaces, ip), ip: ip}
	}
	return defaultRouteFromProc(ifaces)
}

// udpRouteSource asks the kernel which source address it would use for an
// off-link destination. Connecting a UDP socket performs a route lookup without
// sending a packet, which is why this works offline and needs no privileges. The
// destinations are globally routable literals, so name resolution never runs.
func udpRouteSource() net.IP {
	for _, target := range []string{"1.1.1.1:53", "8.8.8.8:53"} {
		conn, err := net.Dial("udp4", target)
		if err != nil {
			continue
		}
		addr, ok := conn.LocalAddr().(*net.UDPAddr)
		_ = conn.Close()
		if !ok || addr == nil {
			continue
		}
		ip := addr.IP.To4()
		if ip == nil || ip.IsUnspecified() || ip.IsLoopback() {
			continue
		}
		return ip
	}
	return nil
}

// defaultRouteFromProc reads the Linux routing table when the UDP probe could
// not answer, for example when every candidate destination already sits on a
// directly connected network. On other platforms the file is absent and this
// returns an empty result.
func defaultRouteFromProc(ifaces []ifaceSnapshot) routeInfo {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return routeInfo{}
	}
	defer file.Close()

	best := ""
	bestMetric := 0
	scanner := bufio.NewScanner(file)
	for first := true; scanner.Scan(); first = false {
		if first {
			continue // header
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&0x1 == 0 { // RTF_UP
			continue
		}
		metric, _ := strconv.Atoi(fields[6])
		if best == "" || metric < bestMetric {
			best = fields[0]
			bestMetric = metric
		}
	}
	if best == "" {
		return routeInfo{}
	}
	return routeInfo{iface: best, ip: firstIPv4ForIface(ifaces, best)}
}

// ifaceNameForIP finds the interface that carries an address.
func ifaceNameForIP(ifaces []ifaceSnapshot, ip net.IP) string {
	for _, iface := range ifaces {
		for _, candidate := range iface.IPs {
			if candidate.To4() != nil && candidate.Equal(ip) {
				return iface.Name
			}
		}
	}
	return ""
}

// firstIPv4ForIface returns the first global IPv4 address on an interface.
func firstIPv4ForIface(ifaces []ifaceSnapshot, name string) net.IP {
	for _, iface := range ifaces {
		if iface.Name != name {
			continue
		}
		for _, candidate := range iface.IPs {
			v4 := candidate.To4()
			if v4 != nil && usableLANIPv4(v4) {
				return v4
			}
		}
	}
	return nil
}
