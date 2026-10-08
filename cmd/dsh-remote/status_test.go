package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/logging"
	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/proxy"
	"github.com/tkoizumi/dsh-remote/internal/systemd"
)

// incidentDiagnose reproduces the state from the incident report: DeepSeek
// Harness still holds its port while the proxy that launched it is gone.
func incidentDiagnose() diagnose {
	return diagnose{
		State: &process.State{
			PID:       118666,
			DSHPID:    118695,
			DSHAddr:   "http://127.0.0.1:3080",
			ProxyAddr: "http://127.0.0.1:3081",
			RemoteURL: "https://ubuntu-server.tail6d6db9.ts.net/dsh",
			LANAddr:   "192.168.0.151",
			LANURL:    "http://192.168.0.151:3081/dsh",
			StartedAt: time.Now(),
		},
		DSH: dshProbe{
			Addr:          "http://127.0.0.1:3080",
			Listening:     true,
			PID:           118695,
			Ours:          true,
			Inspectable:   true,
			StatusCode:    401,
			AuthChallenge: true,
		},
		Proxy: proxyProbe{Addr: "http://127.0.0.1:3081"},
		// The report's Ubuntu server had systemd but no unit installed.
		Supervision: systemd.State{Supported: true, UnitPath: "/home/taka/.config/systemd/user/dsh-remote.service"},
	}
}

func defaultOpts() statusOptions {
	return statusOptions{dshPort: 3080, proxyPort: 3081}
}

// TestClassifyIncidentState is the core regression test: the incident state must
// be recognised as an orphan, not as a plain stopped proxy, because the two need
// different recovery.
func TestClassifyIncidentState(t *testing.T) {
	if got := classify(incidentDiagnose()); got != situationOrphaned {
		t.Fatalf("classify(incident) = %v, want situationOrphaned", got)
	}
}

func TestClassifySituationMatrix(t *testing.T) {
	stopped := diagnose{
		Proxy: proxyProbe{Addr: "http://127.0.0.1:3081"},
		DSH:   dshProbe{Addr: "http://127.0.0.1:3080"},
	}
	if got := classify(stopped); got != situationStopped {
		t.Errorf("classify(nothing running) = %v, want situationStopped", got)
	}

	healthy := diagnose{
		Proxy: proxyProbe{Addr: "http://127.0.0.1:3081", Listening: true, Healthy: true,
			Health: proxy.Health{Status: "ok"}},
		DSH: dshProbe{Addr: "http://127.0.0.1:3080", Listening: true, Ours: true},
	}
	if got := classify(healthy); got != situationHealthy {
		t.Errorf("classify(healthy) = %v, want situationHealthy", got)
	}

	// The proxy is gone and something that is not our child holds the DSH port.
	foreign := diagnose{
		Proxy: proxyProbe{Addr: "http://127.0.0.1:3081"},
		DSH:   dshProbe{Addr: "http://127.0.0.1:3080", Listening: true, PID: 4242},
	}
	if got := classify(foreign); got != situationPortTaken {
		t.Errorf("classify(foreign holder) = %v, want situationPortTaken", got)
	}

	// The proxy port is bound but the health check does not succeed.
	broken := diagnose{
		Proxy: proxyProbe{Addr: "http://127.0.0.1:3081", Listening: true, Err: errHealthMalformed},
		DSH:   dshProbe{Addr: "http://127.0.0.1:3080", Listening: true},
	}
	if got := classify(broken); got != situationProxyUnhealthy {
		t.Errorf("classify(broken proxy) = %v, want situationProxyUnhealthy", got)
	}
}

var errHealthMalformed = &healthError{}

type healthError struct{}

func (*healthError) Error() string { return "health endpoint returned malformed JSON" }

// TestStatusReportsIncidentActionably pins the user-facing contract the report
// asked for: it must say remote access is unavailable, why, that nothing would
// restart the proxy, and what to run.
func TestStatusReportsIncidentActionably(t *testing.T) {
	out := renderStatus(incidentDiagnose(), defaultOpts())

	for _, want := range []string{
		"Remote access: UNAVAILABLE",
		"still holding port 3080",
		"A plain restart would fail",
		"systemd unit: not installed",
		"no automatic restart is configured",
		"dsh-remote reconcile",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output is missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "Remote access: AVAILABLE") {
		t.Errorf("status claimed remote access was available during an outage:\n%s", out)
	}
}

// TestStatusDoesNotPrintStalePIDAsLive guards the confusing case observed live:
// run state named a dead proxy pid while another process held the port.
func TestStatusDoesNotPrintStalePIDAsLive(t *testing.T) {
	d := incidentDiagnose()
	d.DSH.Alive = false
	d.DSH.PID = 0
	d.DSH.Inspectable = false

	out := renderStatus(d, defaultOpts())
	if !strings.Contains(out, "no longer alive") {
		t.Errorf("status did not flag the stale recorded pid:\n%s", out)
	}
	if strings.Contains(out, "DSH PID: 118695\n") {
		t.Errorf("status printed a dead recorded pid as though it were current:\n%s", out)
	}
}

// TestStatusSupervisionReportsRestartPolicy proves an installed unit with an
// unusable policy is not reported as if it would recover.
func TestStatusSupervisionReportsRestartPolicy(t *testing.T) {
	d := incidentDiagnose()
	d.Supervision = systemd.State{
		Supported: true,
		Installed: true,
		UnitPath:  "/home/taka/.config/systemd/user/dsh-remote.service",
		Restart:   "no",
	}

	out := renderStatus(d, defaultOpts())
	if !strings.Contains(out, "Restart=no") {
		t.Errorf("status did not report the restart policy:\n%s", out)
	}
	if !strings.Contains(out, "no automatic recovery is available") {
		t.Errorf("status promised recovery that the policy cannot deliver:\n%s", out)
	}
}

// TestStatusHealthySaysNothingToDo keeps the healthy path short.
func TestStatusHealthySaysNothingToDo(t *testing.T) {
	d := diagnose{
		State: &process.State{PID: 10, DSHPID: 11, RemoteURL: "https://host.tailnet.ts.net/dsh"},
		Proxy: proxyProbe{Addr: "http://127.0.0.1:3081", Listening: true, Healthy: true,
			Health: proxy.Health{Status: "ok", Version: "0.1.8", TokenCaptured: true}},
		DSH: dshProbe{Addr: "http://127.0.0.1:3080", Listening: true, AuthChallenge: true, Alive: true},
	}
	out := renderStatus(d, defaultOpts())
	if !strings.Contains(out, "Remote access: AVAILABLE") {
		t.Errorf("healthy status did not report availability:\n%s", out)
	}
	if !strings.Contains(out, "None required.") {
		t.Errorf("healthy status suggested an unnecessary action:\n%s", out)
	}
}

// TestStatusReportsTheDiagnosticLog proves the report points at the file that
// survives a restart, including how recently it was written -- which is what
// tells an operator whether the proxy died just now or hours ago.
func TestStatusReportsTheDiagnosticLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dsh-remote.log")
	if err := os.WriteFile(logPath, []byte("2026-10-08T13:00:00.000Z shutdown: received terminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSH_REMOTE_STATE_FILE", filepath.Join(dir, "state.json"))

	d := incidentDiagnose()
	d.Log = logging.Inspect(logPath)

	out := renderStatus(d, defaultOpts())
	if !strings.Contains(out, logPath) {
		t.Errorf("status did not name the log path:\n%s", out)
	}
	if !strings.Contains(out, "last entry: ") {
		t.Errorf("status did not show the last log entry:\n%s", out)
	}
	if !strings.Contains(out, "shutdown: received terminated") {
		t.Errorf("status did not surface how the last run ended:\n%s", out)
	}
}

// TestDoctorReportsTheLog covers the doctor half of the same guarantee.
func TestDoctorReportsTheLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dsh-remote.log")
	if err := os.WriteFile(logPath, []byte("started\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := doctorWith(diagnose{Log: logging.Inspect(logPath)}, systemd.State{Supported: true})
	c := checkLog(d)
	if c.Status != checkOK {
		t.Fatalf("log check status = %v, want checkOK: %+v", c.Status, c)
	}
	if !strings.Contains(c.Detail, logPath) {
		t.Errorf("log check did not name the path: %q", c.Detail)
	}
	if !strings.Contains(c.Advice, "tail -n") {
		t.Errorf("log check advice is not actionable: %q", c.Advice)
	}
}

// TestDoctorLogMissingIsQuiet keeps a first run from looking like a fault.
func TestDoctorLogMissingIsQuiet(t *testing.T) {
	c := checkLog(diagnose{Log: logging.Status{Path: "/tmp/none/dsh-remote.log"}})
	if c.Status != checkSkip {
		t.Fatalf("missing log status = %v, want checkSkip", c.Status)
	}
}

// TestDiagnosticsNeverLeakToken is a guard on the security requirement: no
// diagnostic output may contain a token or cookie value.
func TestDiagnosticsNeverLeakToken(t *testing.T) {
	const secret = "ykUIFiyl2GcfrTIaUAth_iT7l_yZZkKhjvEABM6WxAo"

	d := incidentDiagnose()
	// Simulate a status pass that somehow observed a live token-bound redirect.
	d.Proxy = proxyProbe{Addr: "http://127.0.0.1:3081", Listening: true, Healthy: true,
		Health: proxy.Health{Status: "ok", Version: "0.1.8", TokenCaptured: true}}
	d.DSH.CookieIssued = true

	outputs := map[string]string{
		"status": renderStatus(d, defaultOpts()),
		"doctor": renderChecks(buildChecks(d, defaultOpts()), d, defaultOpts()),
	}
	for name, out := range outputs {
		if strings.Contains(out, "token="+secret) {
			t.Errorf("%s output contains a token-bound URL:\n%s", name, out)
		}
		if strings.Contains(out, secret) {
			t.Errorf("%s output contains the token itself:\n%s", name, out)
		}
		if strings.Contains(strings.ToLower(out), "set-cookie") {
			t.Errorf("%s output contains a cookie header:\n%s", name, out)
		}
	}
}

// TestDoctorFlagsOrphan is the doctor half of the incident regression: the
// orphan must be its own failing check with the matching remedy.
func TestDoctorFlagsOrphan(t *testing.T) {
	d := doctorWith(incidentDiagnose(), systemd.State{Supported: true})
	checks := buildChecks(d, defaultOpts())

	var orphan *check
	for i := range checks {
		if checks[i].Name == "orphan check" {
			orphan = &checks[i]
		}
	}
	if orphan == nil {
		t.Fatal("doctor did not run an orphan check")
	}
	if orphan.Status != checkFail {
		t.Fatalf("orphan check status = %v, want checkFail", orphan.Status)
	}
	if !strings.Contains(orphan.Advice, "dsh-remote start") {
		t.Errorf("orphan advice does not point at the fix: %q", orphan.Advice)
	}
	if !anyFailed(checks) {
		t.Fatal("the incident state produced no failing check")
	}
}

// TestDoctorFlagsStaleState covers the live-observed case: a healthy proxy but a
// recorded pid that is no longer alive.
func TestDoctorFlagsStaleState(t *testing.T) {
	d := diagnose{
		State: &process.State{PID: 1 << 30, DSHPID: 1 << 30, StartedAt: time.Now()},
		Proxy: proxyProbe{Addr: "http://127.0.0.1:3081", Listening: true, Healthy: true,
			Health: proxy.Health{Status: "ok"}},
		DSH: dshProbe{Addr: "http://127.0.0.1:3080", Listening: true, AuthChallenge: true},
	}
	checks := buildChecks(d, defaultOpts())
	for _, c := range checks {
		if c.Name != "run state" {
			continue
		}
		if c.Status != checkWarn {
			t.Fatalf("stale run state status = %v, want checkWarn", c.Status)
		}
		if !strings.Contains(c.Detail, "stale") {
			t.Fatalf("stale run state detail does not say so: %q", c.Detail)
		}
		return
	}
	t.Fatal("doctor did not run a run-state check")
}

// TestDoctorSupervisionAdviceIsActionable checks the installed-but-stopped case
// points at the exact command.
func TestDoctorSupervisionAdviceIsActionable(t *testing.T) {
	d := doctorWith(diagnose{}, systemd.State{
		Supported: true,
		Installed: true,
		UnitPath:  "/home/taka/.config/systemd/user/dsh-remote.service",
		Restart:   "always",
	})
	c := checkSupervision(d)
	if c.Status != checkWarn {
		t.Fatalf("inactive unit status = %v, want checkWarn", c.Status)
	}
	if !strings.Contains(c.Advice, "systemctl --user start "+systemd.UnitName) {
		t.Fatalf("supervision advice is not actionable: %q", c.Advice)
	}
}

// TestRenderChecksVerdictUnavailable proves the verdict line reports the outage
// and not just a list of warnings.
func TestRenderChecksVerdictUnavailable(t *testing.T) {
	d := doctorWith(incidentDiagnose(), systemd.State{Supported: true})
	out := renderChecks(buildChecks(d, defaultOpts()), d, defaultOpts())
	if !strings.Contains(out, "UNAVAILABLE") {
		t.Errorf("doctor verdict did not report the outage:\n%s", out)
	}
	if !strings.Contains(out, "orphaned-dsh") {
		t.Errorf("doctor verdict did not name the failure mode:\n%s", out)
	}
}

// TestDescribeExitAlwaysNamesACause is the regression test for a real defect seen
// in a supervised run: a failed start logged
//
//	shutdown: unknown (teardown ran without recording a reason)
//
// for a failure whose reason was already known, which reads like an unexplained
// crash in exactly the file that exists to prevent one.
func TestDescribeExitAlwaysNamesACause(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		cause  error
		want   string
	}{
		{"explicit reason wins", "received terminated", nil, "received terminated"},
		{"preflight refusal is named", "", errors.New("port 3080 is in use by pid 161813"), "failed: port 3080 is in use by pid 161813"},
		{"bind failure is named", "", errors.New("port 3091 is not available on 127.0.0.1"), "failed: port 3091 is not available on 127.0.0.1"},
		{"explicit reason beats the error", "the stable proxy stopped", errors.New("listener closed"), "the stable proxy stopped"},
	}
	for _, tc := range cases {
		got := describeExit(tc.reason, tc.cause)
		if got != tc.want {
			t.Errorf("%s: describeExit(%q, %v) = %q, want %q", tc.name, tc.reason, tc.cause, got, tc.want)
		}
		if strings.Contains(got, "unknown") {
			t.Errorf("%s: a known failure was recorded as unknown: %q", tc.name, got)
		}
	}

	// The only case that may read as unknown is one with genuinely nothing to
	// report, and it must say so explicitly rather than looking like a crash.
	if got := describeExit("", nil); !strings.Contains(got, "unknown") {
		t.Errorf("describeExit with no reason and no error = %q, want an explicit unknown", got)
	}
}
