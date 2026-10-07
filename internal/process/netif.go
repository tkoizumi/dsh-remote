package process

import (
	"net"
	"sort"
)

// cgnat is the carrier-grade NAT range. Tailscale hands out addresses from it,
// and Tailscale already has its own path onto this host, so those addresses are
// not what "the local network" means.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// LANAddresses returns this host's non-loopback IPv4 addresses, sorted. It
// excludes link-local addresses and the Tailscale interface so callers can
// treat the result as "addresses reachable from the local network".
func LANAddresses() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip) {
				continue
			}
			out = append(out, ip.String())
		}
	}
	sort.Strings(out)
	return out
}
