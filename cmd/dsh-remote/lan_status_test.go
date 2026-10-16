package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/proxy"
	"github.com/tkoizumi/dsh-remote/internal/systemd"
)

// healthyWithLAN builds a diagnosis whose loopback proxy is healthy and whose
// recorded LAN endpoint has the supplied probe result.
func healthyWithLAN(lan lanProbe) diagnose {
	return diagnose{
		State: &process.State{
			PID:       os.Getpid(),
			DSHPID:    os.Getpid(),
			DSHAddr:   "http://127.0.0.1:3080",
			ProxyAddr: "http://127.0.0.1:3081",
			LANAddr:   "192.168.0.151",
			LANURL:    "http://192.168.0.151:3081/dsh",
			StartedAt: time.Now(),
		},
		Proxy: proxyProbe{Addr: "http://127.0.0.1:3081", Listening: true, Healthy: true,
			Health: proxy.Health{Status: "ok", TokenCaptured: true}},
		DSH: dshProbe{Addr: "http://127.0.0.1:3080", Listening: true, AuthChallenge: true},
		LAN: lan,
	}
}

// TestStatusAdvertisesReachableLANEndpoint pins the expected output from the
// report: the machine's real LAN address must be what is advertised, and the
// report must say the endpoint actually answers.
func TestStatusAdvertisesReachableLANEndpoint(t *testing.T) {
	d := healthyWithLAN(lanProbe{
		Configured: true,
		Addr:       "192.168.0.151:3081",
		URL:        "http://192.168.0.151:3081/dsh",
		Probed:     true,
		Reachable:  true,
		IsProxy:    true,
		Status:     "302 Found",
	})

	out := renderStatus(d, defaultOpts())
	for _, want := range []string{
		"Local network DSH (no Tailscale required):",
		"http://192.168.0.151:3081/dsh",
		"reachable from this host on the LAN address",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status is missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "172.17.0.1") {
		t.Errorf("status advertised the Docker bridge:\n%s", out)
	}
}

// TestStatusDistinguishesUnreachableLANFromHealthyProxy is the other half of the
// requirement: a healthy loopback proxy must not make an absent LAN listener
// look fine.
func TestStatusDistinguishesUnreachableLANFromHealthyProxy(t *testing.T) {
	d := healthyWithLAN(lanProbe{
		Configured: true,
		Addr:       "192.168.0.151:3081",
		URL:        "http://192.168.0.151:3081/dsh",
		Probed:     true,
		Err:        &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded},
	})

	out := renderStatus(d, defaultOpts())
	if !strings.Contains(out, "NOT reachable from this host on 192.168.0.151:3081") {
		t.Errorf("status did not flag the unreachable LAN endpoint:\n%s", out)
	}
	if !strings.Contains(out, "Proxy: running") {
		t.Errorf("status did not still report the loopback proxy as running:\n%s", out)
	}
	if !strings.Contains(out, "dsh-remote stop && dsh-remote start --lan") {
		t.Errorf("status did not suggest how to rebind the LAN endpoint:\n%s", out)
	}
}

// TestStatusFlagsLoopbackLANAddress covers an explicit override that cannot
// serve a LAN at all.
func TestStatusFlagsLoopbackLANAddress(t *testing.T) {
	d := healthyWithLAN(lanProbe{
		Configured: true,
		Addr:       "127.0.0.1:3081",
		URL:        "http://127.0.0.1:3081/dsh",
		Loopback:   true,
	})
	out := renderStatus(d, defaultOpts())
	if !strings.Contains(out, "NOT usable: the recorded address is loopback") {
		t.Errorf("status did not flag a loopback LAN address:\n%s", out)
	}
}

// TestStatusFlagsDockerBridgeLANAddress reproduces the reported output after a
// fix: an old state file records 172.17.0.1, which answers locally but is a
// Docker bridge. The report must not call that a working LAN endpoint.
func TestStatusFlagsDockerBridgeLANAddress(t *testing.T) {
	d := healthyWithLAN(lanProbe{
		Configured: true,
		Addr:       "172.17.0.1:3081",
		URL:        "http://172.17.0.1:3081/dsh",
		Probed:     true,
		Reachable:  true,
		IsProxy:    true,
		NonLAN:     true,
	})
	d.State.LANAddr = "172.17.0.1"
	d.State.LANURL = "http://172.17.0.1:3081/dsh"

	out := renderStatus(d, defaultOpts())
	if !strings.Contains(out, "NOT a local network address") {
		t.Errorf("status called a Docker bridge a local network endpoint:\n%s", out)
	}
	if strings.Contains(out, "reachable from this host on the LAN address") {
		t.Errorf("status claimed the Docker bridge was LAN-reachable:\n%s", out)
	}
}

// TestDoctorLANChecks exercises every LAN check outcome directly.
func TestDoctorLANChecks(t *testing.T) {
	cases := []struct {
		name   string
		lan    lanProbe
		status checkStatus
		want   string
	}{
		{"not configured", lanProbe{}, checkSkip, "no local network endpoint"},
		{"loopback", lanProbe{Configured: true, Addr: "127.0.0.1:3081", Loopback: true}, checkFail, "loopback"},
		{"docker bridge", lanProbe{Configured: true, Addr: "172.17.0.1:3081", Probed: true, Reachable: true, IsProxy: true, NonLAN: true}, checkFail, "not on an interface"},
		{"proxy answers", lanProbe{Configured: true, Addr: "192.168.0.151:3081", Probed: true, Reachable: true, IsProxy: true, Status: "302 Found"}, checkOK, "answers as the proxy"},
		{"foreign listener", lanProbe{Configured: true, Addr: "192.168.0.151:3081", Probed: true, Reachable: true, Status: "200 OK"}, checkWarn, "not as the dsh-remote proxy"},
		{"unreachable", lanProbe{Configured: true, Addr: "192.168.0.151:3081", Probed: true}, checkFail, "not reachable"},
	}
	for _, tc := range cases {
		got := checkLAN(healthyWithLAN(tc.lan), defaultOpts())
		if got.Status != tc.status {
			t.Errorf("%s: checkLAN status = %v, want %v (%s)", tc.name, got.Status, tc.status, got.Detail)
		}
		if !strings.Contains(got.Detail, tc.want) {
			t.Errorf("%s: checkLAN detail = %q, want it to contain %q", tc.name, got.Detail, tc.want)
		}
	}
}

// TestProbeLANDiscoversProxyOnLANAddress binds a listener on this host's real
// LAN address (skipping when there is none) and proves the probe recognises the
// proxy's redirect there rather than treating the address as unreachable.
func TestProbeLANDiscoversProxyOnLANAddress(t *testing.T) {
	addresses := process.LANAddresses()
	if len(addresses) == 0 {
		t.Skip("no LAN address on this machine")
	}
	lanIP := addresses[0]
	ln, err := proxy.ListenOn(lanIP, 0)
	if err != nil {
		t.Skipf("cannot bind %s: %v", lanIP, err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/?token=REDACTED", http.StatusFound)
	})}
	go func() { _ = server.Serve(ln) }()
	defer func() { _ = server.Close() }()

	port := ln.Addr().(*net.TCPAddr).Port
	state := &process.State{
		LANAddr: lanIP,
		LANURL:  "http://" + net.JoinHostPort(lanIP, strconv.Itoa(port)) + proxy.BootstrapPath,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	probe := probeLAN(ctx, state, port)
	if !probe.Probed || !probe.Reachable || !probe.IsProxy {
		t.Fatalf("probeLAN(%s) = %+v, want a reachable proxy", state.LANURL, probe)
	}
}

// TestProbeLANWithoutStateIsUnconfigured keeps a run that never enabled --lan
// from being reported as a fault.
func TestProbeLANWithoutStateIsUnconfigured(t *testing.T) {
	if got := probeLAN(context.Background(), nil, 3081); got.Configured || got.Probed {
		t.Fatalf("probeLAN(nil) = %+v, want unconfigured", got)
	}
}

// TestVerifyLANListenerRejectsLoopback stops a run from advertising a loopback
// address as the local network endpoint, whatever the flag said.
func TestVerifyLANListenerRejectsLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := verifyLANListener(ln, "127.0.0.1", port, nil); err == nil {
		t.Fatal("verifyLANListener accepted a loopback listener as a LAN endpoint")
	}
}

// TestVerifyLANListenerAcceptsRealLANAddress proves the self-dial succeeds for a
// listener bound to this host's real LAN address (skipped when there is none).
func TestVerifyLANListenerAcceptsRealLANAddress(t *testing.T) {
	addresses := process.LANAddresses()
	if len(addresses) == 0 {
		t.Skip("no LAN address on this machine")
	}
	ln, err := proxy.ListenOn(addresses[0], 0)
	if err != nil {
		t.Skipf("cannot bind %s: %v", addresses[0], err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := verifyLANListener(ln, addresses[0], port, nil); err != nil {
		t.Fatalf("verifyLANListener(%s) = %v, want nil", addresses[0], err)
	}
}

// TestStatusWarnsWhenUnitInactiveButProxyRuns is the lifecycle half of the
// report: systemd shows the unit inactive while an unmanaged proxy is serving.
// The status must say so, and must not claim systemd would recover it.
func TestStatusWarnsWhenUnitInactiveButProxyRuns(t *testing.T) {
	d := healthyWithLAN(lanProbe{Configured: true, Addr: "192.168.0.151:3081", Probed: true, Reachable: true, IsProxy: true})
	d.Supervision = systemd.State{
		Supported: true,
		Installed: true,
		UnitPath:  "/home/taka/.config/systemd/user/dsh-remote.service",
		Restart:   "on-failure",
	}

	out := renderStatus(d, defaultOpts())
	if !strings.Contains(out, "a proxy is running that systemd did not start") {
		t.Errorf("status did not flag the unmanaged running proxy:\n%s", out)
	}
	if !strings.Contains(out, "dsh-remote stop && systemctl --user start "+systemd.UnitName) {
		t.Errorf("status did not explain how to hand the instance back to systemd:\n%s", out)
	}

	c := checkSupervision(d)
	if c.Status != checkWarn {
		t.Fatalf("checkSupervision status = %v, want checkWarn", c.Status)
	}
	if !strings.Contains(c.Detail, "outside systemd") {
		t.Errorf("doctor detail does not describe the mismatch: %q", c.Detail)
	}
}

// TestStatusDoesNotWarnWhenUnitIsActive keeps the warning specific.
func TestStatusDoesNotWarnWhenUnitIsActive(t *testing.T) {
	d := healthyWithLAN(lanProbe{Configured: true, Probed: true, Reachable: true, IsProxy: true})
	d.Supervision = systemd.State{
		Supported: true,
		Installed: true,
		Active:    true,
		UnitPath:  "/home/taka/.config/systemd/user/dsh-remote.service",
		Restart:   "on-failure",
	}
	out := renderStatus(d, defaultOpts())
	if strings.Contains(out, "did not start") {
		t.Errorf("status warned about an unmanaged proxy while the unit is active:\n%s", out)
	}
}
