package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/proxy"
)

func TestParseServiceSpecs(t *testing.T) {
	valid := []struct {
		name  string
		input []string
		want  []serviceSpec
	}{
		{
			name:  "explicit loopback host and port",
			input: []string{"hermes=127.0.0.1:9119"},
			want:  []serviceSpec{{Name: "hermes", Host: "127.0.0.1", Port: 9119}},
		},
		{
			name:  "port shorthand defaults to loopback",
			input: []string{"hermes=9119"},
			want:  []serviceSpec{{Name: "hermes", Host: "127.0.0.1", Port: 9119}},
		},
		{
			name:  "empty host defaults to loopback",
			input: []string{"hermes=:9119"},
			want:  []serviceSpec{{Name: "hermes", Host: "127.0.0.1", Port: 9119}},
		},
		{
			name:  "localhost is canonicalised to 127.0.0.1",
			input: []string{"hermes=localhost:9119"},
			want:  []serviceSpec{{Name: "hermes", Host: "127.0.0.1", Port: 9119}},
		},
		{
			name:  "ipv6 loopback is canonicalised",
			input: []string{"hermes=[::1]:9119"},
			want:  []serviceSpec{{Name: "hermes", Host: "127.0.0.1", Port: 9119}},
		},
		{
			name:  "two distinct services",
			input: []string{"hermes=9119", "grafana=3000"},
			want: []serviceSpec{
				{Name: "hermes", Host: "127.0.0.1", Port: 9119},
				{Name: "grafana", Host: "127.0.0.1", Port: 3000},
			},
		},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseServiceSpecs(tc.input)
			if err != nil {
				t.Fatalf("parseServiceSpecs(%v) = %v", tc.input, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseServiceSpecs(%v) = %+v, want %+v", tc.input, got, tc.want)
			}
			if got[0].target() != "http://127.0.0.1:9119" {
				t.Fatalf("target = %q, want the canonical loopback URL", got[0].target())
			}
		})
	}

	invalid := map[string][]string{
		"missing equals":    {"hermes"},
		"missing name":      {"=127.0.0.1:9119"},
		"missing target":    {"hermes="},
		"bad port":          {"hermes=127.0.0.1:notaport"},
		"port zero":         {"hermes=127.0.0.1:0"},
		"port too large":    {"hermes=127.0.0.1:70000"},
		"remote host":       {"hermes=10.0.0.5:9119"},
		"duplicate name":    {"hermes=9119", "hermes=9120"},
		"duplicate port":    {"hermes=9119", "grafana=9119"},
		"too many services": {"a=1", "b=2", "c=3"},
		"bare ipv6 no port": {"hermes=::1"},
	}
	for name, input := range invalid {
		t.Run(name, func(t *testing.T) {
			if got, err := parseServiceSpecs(input); err == nil {
				t.Fatalf("parseServiceSpecs(%v) = %+v, want an error", input, got)
			}
		})
	}
}

// TestInstallStartFlagsServices proves the supervised unit carries the extra
// dashboard exactly as declared.
func TestInstallStartFlagsServices(t *testing.T) {
	got := installStartFlags(true, "", false, "", nil, []string{"hermes=127.0.0.1:9119"})
	for _, want := range []string{" --lan", " --service=hermes=127.0.0.1:9119"} {
		if !strings.Contains(got, want) {
			t.Fatalf("installStartFlags = %q, missing %q", got, want)
		}
	}
}

// TestStatusReportsServiceEndpoints pins the two URLs a user asked for and the
// independent verification of each.
func TestStatusReportsServiceEndpoints(t *testing.T) {
	d := diagnose{
		State: &process.State{Services: []process.ServiceState{{
			Name:       "hermes",
			Target:     "http://127.0.0.1:9119",
			HTTPSPort:  8443,
			TailnetURL: "https://ubuntu-server.tail6d6db9.ts.net:8443/",
			LANURL:     "http://192.168.0.151:9119/",
		}}},
		Services: []serviceProbe{{
			Name:            "hermes",
			Target:          "http://127.0.0.1:9119",
			HTTPSPort:       8443,
			TailnetURL:      "https://ubuntu-server.tail6d6db9.ts.net:8443/",
			LANURL:          "http://192.168.0.151:9119/",
			LANProbed:       true,
			LANReachable:    true,
			LANStatus:       "200 OK",
			TailServeTarget: "http://127.0.0.1:9119",
		}},
	}

	out := renderStatus(d, defaultOpts())
	for _, want := range []string{
		"Local services:",
		"hermes:",
		"https://ubuntu-server.tail6d6db9.ts.net:8443/",
		"http://192.168.0.151:9119/",
		"tailscale mapping: ok",
		"reachable (200 OK)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status is missing %q\n---\n%s", want, out)
		}
	}
}

// TestDoctorServiceChecks covers the outcomes of the per-service check.
func TestDoctorServiceChecks(t *testing.T) {
	cases := []struct {
		name    string
		probe   serviceProbe
		status  checkStatus
		want    string
		advice  string
		notWant string
	}{
		{
			name:   "healthy on both paths",
			probe:  serviceProbe{Name: "hermes", Target: "http://127.0.0.1:9119", HTTPSPort: 8443, TailnetURL: "https://host:8443/", LANURL: "http://192.168.0.151:9119/", TailServeTarget: "http://127.0.0.1:9119", LANProbed: true, LANReachable: true},
			status: checkOK,
			want:   "tailnet: https://host:8443/",
		},
		{
			name:   "missing serve mapping",
			probe:  serviceProbe{Name: "hermes", Target: "http://127.0.0.1:9119", HTTPSPort: 8443, TailnetURL: "https://host:8443/"},
			status: checkFail,
			want:   "no Serve mapping on port 8443",
			// The advice must be the copy-pasteable flag form, not the URL form.
			advice: "--service hermes=127.0.0.1:9119",
		},
		{
			name:   "lan endpoint down",
			probe:  serviceProbe{Name: "hermes", Target: "http://127.0.0.1:9119", LANURL: "http://192.168.0.151:9119/", LANProbed: true, LANErr: errHealthMalformed},
			status: checkFail,
			want:   "is not reachable",
		},
		{
			name:   "docker bridge address",
			probe:  serviceProbe{Name: "hermes", Target: "http://127.0.0.1:9119", LANURL: "http://172.17.0.1:9119/", LANNonLAN: true},
			status: checkFail,
			want:   "not on a LAN-reachable interface",
		},
		{
			// The reported case: the LAN forwarder answers 502 because the
			// dashboard is down. It must fail as a dead backend, once, not be
			// reported as a working endpoint.
			name: "dead backend",
			probe: serviceProbe{
				Name: "hermes", Target: "http://127.0.0.1:9119", HTTPSPort: 8443,
				TailnetURL: "https://host:8443/", TailServeTarget: "http://127.0.0.1:9119",
				LANURL: "http://192.168.0.151:9119/", LANProbed: true, LANErr: errors.New("the dashboard backend at http://127.0.0.1:9119 is not reachable"),
				BackendProbed: true, BackendErr: errors.New("dial tcp 127.0.0.1:9119: connect: connection refused"),
			},
			status:  checkFail,
			want:    "backend http://127.0.0.1:9119 is not reachable",
			advice:  "start the dashboard",
			notWant: "local network http://192.168.0.151:9119/ is not reachable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checks := checkServices(diagnose{Services: []serviceProbe{tc.probe}})
			if len(checks) != 1 {
				t.Fatalf("checkServices returned %d checks, want 1", len(checks))
			}
			if checks[0].Status != tc.status {
				t.Errorf("status = %v, want %v (%s)", checks[0].Status, tc.status, checks[0].Detail)
			}
			if !strings.Contains(checks[0].Detail, tc.want) {
				t.Errorf("detail = %q, want it to contain %q", checks[0].Detail, tc.want)
			}
			if tc.advice != "" && !strings.Contains(checks[0].Advice, tc.advice) {
				t.Errorf("advice = %q, want it to contain %q", checks[0].Advice, tc.advice)
			}
			if tc.notWant != "" && strings.Contains(checks[0].Detail, tc.notWant) {
				t.Errorf("detail = %q, must not repeat %q", checks[0].Detail, tc.notWant)
			}
		})
	}
}

// TestProbeServicesDetectsDeadBackend proves a dashboard that is not listening
// is reported as down, independently of the LAN listener.
func TestProbeServicesDetectsDeadBackend(t *testing.T) {
	state := &process.State{Services: []process.ServiceState{{
		Name:   "hermes",
		Target: "http://127.0.0.1:1",
	}}}
	got := probeServices(context.Background(), state, true)
	if len(got) != 1 {
		t.Fatalf("probeServices returned %d probes, want 1", len(got))
	}
	if !got[0].BackendProbed || got[0].BackendReachable {
		t.Fatalf("backend probe = %+v, want probed and unreachable", got[0])
	}
	if got[0].BackendErr == nil {
		t.Fatalf("backend probe did not record an error")
	}
}

// TestProbeServicesTreatsForwarder502AsBackendDown is the regression for the
// reported screenshot: dsh-remote's own 502 must not read as "reachable".
func TestProbeServicesTreatsForwarder502AsBackendDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(proxy.UpstreamUnreachableHeader, proxy.UpstreamUnreachableValue)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "dsh-remote: could not reach http://127.0.0.1:9119\n")
	}))
	defer srv.Close()

	state := &process.State{Services: []process.ServiceState{{
		Name:   "hermes",
		Target: "http://127.0.0.1:1",
		LANURL: srv.URL + "/",
	}}}
	got := probeServices(context.Background(), state, true)
	if len(got) != 1 {
		t.Fatalf("probeServices returned %d probes, want 1", len(got))
	}
	if !got[0].LANProbed || got[0].LANReachable {
		t.Fatalf("LAN probe = %+v, want probed and not reachable", got[0])
	}
	if got[0].LANErr == nil || !strings.Contains(got[0].LANErr.Error(), "not reachable") {
		t.Fatalf("LAN probe error = %v, want a backend-unreachable explanation", got[0].LANErr)
	}
}

// TestStatusDoesNotCallADeadBackendReachable is the user-facing half of the
// regression.
func TestStatusDoesNotCallADeadBackendReachable(t *testing.T) {
	d := diagnose{
		State: &process.State{Services: []process.ServiceState{{
			Name: "hermes", Target: "http://127.0.0.1:9119", HTTPSPort: 8443,
			TailnetURL: "https://host:8443/", LANURL: "http://192.168.0.151:9119/",
		}}},
		Services: []serviceProbe{{
			Name: "hermes", Target: "http://127.0.0.1:9119", HTTPSPort: 8443,
			TailnetURL: "https://host:8443/", LANURL: "http://192.168.0.151:9119/",
			TailServeTarget: "http://127.0.0.1:9119",
			LANProbed:       true, LANErr: errors.New("the dashboard backend at http://127.0.0.1:9119 is not reachable"),
			BackendProbed: true, BackendErr: errors.New("dial tcp 127.0.0.1:9119: connect: connection refused"),
		}},
	}
	out := renderStatus(d, defaultOpts())
	if strings.Contains(out, "reachable (502") {
		t.Errorf("status called a 502-to-dead-backend reachable:\n%s", out)
	}
	for _, want := range []string{
		"local network: NOT reachable",
		"backend http://127.0.0.1:9119: not reachable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status is missing %q\n---\n%s", want, out)
		}
	}
}
