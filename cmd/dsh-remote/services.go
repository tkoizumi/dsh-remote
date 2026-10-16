package main

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/logging"
	"github.com/tkoizumi/dsh-remote/internal/proxy"
	"github.com/tkoizumi/dsh-remote/internal/tailscale"
)

// serviceSpec is one `--service name=host:port` declaration: an additional
// loopback dashboard dsh-remote should expose on the local network and, when
// Serve is managed, on a dedicated tailnet HTTPS port.
type serviceSpec struct {
	// Name labels the service in output and diagnostics.
	Name string
	// Host is the backend host; it must be loopback.
	Host string
	// Port is the backend port, and also the port the LAN listener uses.
	Port int
}

// target is the loopback URL the dashboard already serves.
func (s serviceSpec) target() string {
	return "http://" + net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
}

// serviceServePorts are the HTTPS ports Tailscale Serve can provision besides
// the default 443, which dsh-remote already uses for DSH. One dashboard per
// port keeps every dashboard at its own root, so absolute asset URLs work.
var serviceServePorts = []int{8443, 10000}

// parseServiceSpecs validates the repeatable --service flag. It accepts
// `name=host:port`, `name=:port`, and the `name=port` shorthand for loopback.
func parseServiceSpecs(values []string) ([]serviceSpec, error) {
	var out []serviceSpec
	seenName := make(map[string]bool)
	seenPort := make(map[int]bool)

	for _, raw := range values {
		name, target, ok := strings.Cut(raw, "=")
		name, target = strings.TrimSpace(name), strings.TrimSpace(target)
		if !ok || name == "" || target == "" {
			return nil, fmt.Errorf("--service %q must be name=host:port, for example --service hermes=127.0.0.1:9119", raw)
		}

		host, portText := "127.0.0.1", target
		if h, p, err := net.SplitHostPort(target); err == nil {
			host, portText = h, p
		} else if strings.Contains(target, ":") {
			// Something colon-shaped that does not parse, such as a bare IPv6
			// literal without a port. Report it instead of guessing.
			return nil, fmt.Errorf("--service %s: %q is not a valid host:port target", name, target)
		}
		if host == "" {
			host = "127.0.0.1"
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("--service %s: %q is not a valid TCP port", name, portText)
		}
		if !isLoopbackHost(host) {
			return nil, fmt.Errorf("--service %s: target %s is not loopback; Tailscale Serve proxies only to the local host, and dsh-remote will not forward a remote backend", name, host)
		}
		// Tailscale Serve proxies only to 127.0.0.1, so canonicalise every
		// accepted loopback spelling. The LAN forwarder then targets the same
		// address the tailnet URL does.
		host = "127.0.0.1"
		if seenName[name] {
			return nil, fmt.Errorf("--service %s is declared twice", name)
		}
		if seenPort[port] {
			return nil, fmt.Errorf("--service %s: port %d is already used by another --service", name, port)
		}
		seenName[name] = true
		seenPort[port] = true
		out = append(out, serviceSpec{Name: name, Host: host, Port: port})
	}

	if len(out) > len(serviceServePorts) {
		return nil, fmt.Errorf("at most %d --service dashboards are supported: Tailscale Serve can expose HTTPS on 443, 8443, and 10000, and 443 belongs to DSH", len(serviceServePorts))
	}
	return out, nil
}

// isLoopbackHost reports whether a target host is the local machine.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// servicePath is the Serve mount for a dashboard. It is the root so the
// dashboard's absolute asset URLs keep working.
const servicePath = "/"

// serviceFlagArg renders a recorded target back into the host:port form the
// --service flag accepts, so advice is copy-pasteable.
func serviceFlagArg(target string) string {
	if parsed, err := url.Parse(target); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return target
}

// runningService is a --service being exposed by the current run.
type runningService struct {
	spec        serviceSpec
	httpsPort   int
	forwarder   *proxy.Forwarder
	listener    net.Listener
	priorTarget string
	serveSet    bool
}

// restoreServeSlot puts back whatever occupied a Serve slot before dsh-remote
// took it, or removes the handler dsh-remote created. It never touches another
// port or path, and never runs `tailscale serve reset`.
func restoreServeSlot(ts *tailscale.Client, port int, path, priorTarget string, log *logging.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if priorTarget != "" {
		if err := ts.SetServeProxy(ctx, port, path, priorTarget); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not restore the previous tailscale serve mapping on port %d (%s): %v\n", port, priorTarget, err)
			if log != nil {
				log.Logf("tailscale: could not restore serve %s on port %d -> %s: %v", path, port, priorTarget, err)
			}
		} else if log != nil {
			log.Logf("tailscale: restored serve %s on port %d -> %s", path, port, priorTarget)
		}
		return
	}
	if err := ts.ClearServe(ctx, port, path); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove the tailscale serve mapping on port %d: %v\n", port, err)
		if log != nil {
			log.Logf("tailscale: could not clear serve %s on port %d: %v", path, port, err)
		}
	} else if log != nil {
		log.Logf("tailscale: cleared serve %s on port %d", path, port)
	}
}
