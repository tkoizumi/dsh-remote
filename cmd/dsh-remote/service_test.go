package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/systemd"
)

// startFlagsFor parses args through the real `start` flag set, so the handoff
// decision is exercised against the defaults the command actually uses.
func startFlagsFor(t *testing.T, args ...string) (startOptions, []string) {
	t.Helper()
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	var opts startOptions
	registerStartFlags(fs, &opts)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return opts, serviceFlagsSet(fs, opts)
}

// testExecutable is the path a unit would have to name for a handoff to be
// considered: the binary running this test.
func testExecutable(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve the test binary: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return self
}

// TestServiceFlagsSetIsAboutConfigurationNotSyntax pins the rule that makes a
// bare `dsh-remote start` mean "start the service as installed". Flags that do
// not shape the service -- a QR code, a token timeout -- must not turn a plain
// start into a configuration request, and passing a flag at its default value is
// not a request for anything different either.
func TestServiceFlagsSetIsAboutConfigurationNotSyntax(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"bare start is a wildcard", nil, nil},
		{"presentation flags do not shape the service", []string{"--no-qr", "--token-timeout=30s"}, nil},
		{"default port is not a change", []string{"--dsh-port=3080", "--proxy-port=3081"}, nil},
		{"a different dsh port is", []string{"--dsh-port=3090"}, []string{"dsh-port"}},
		{"a different proxy port is", []string{"--proxy-port=3099"}, []string{"proxy-port"}},
		{"--lan is", []string{"--lan"}, []string{"lan"}},
		{"--service is", []string{"--service=hermes=127.0.0.1:9119"}, []string{"service"}},
		{"--foreground is not", []string{"--foreground"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, set := startFlagsFor(t, tc.args...)
			if !slices.Equal(set, tc.want) {
				t.Fatalf("serviceFlagsSet(%v) = %q, want %q", tc.args, set, tc.want)
			}
		})
	}
}

// TestServiceRunsThisInvocation is the guard that decides whether `start` drives
// the unit or refuses. Getting it wrong in the permissive direction is how a
// foreground proxy ends up racing an enabled unit -- the restart storm.
func TestServiceRunsThisInvocation(t *testing.T) {
	self := testExecutable(t)

	cases := []struct {
		name     string
		unitExe  string
		unitArgs []string
		args     []string
		want     bool
	}{
		{
			name:     "bare start matches whatever the unit runs",
			unitExe:  self,
			unitArgs: []string{"start", "--lan"},
			args:     nil,
			want:     true,
		},
		{
			name:     "explicit flags matching the unit",
			unitExe:  self,
			unitArgs: []string{"start", "--lan"},
			args:     []string{"--lan"},
			want:     true,
		},
		{
			name:     "explicit flags the unit does not have",
			unitExe:  self,
			unitArgs: []string{"start", "--lan"},
			args:     []string{"--no-serve"},
			want:     false,
		},
		{
			name:     "the unit has flags this invocation did not ask for",
			unitExe:  self,
			unitArgs: []string{"start", "--lan", "--service=hermes=127.0.0.1:9119"},
			args:     []string{"--lan"},
			want:     false,
		},
		{
			name:     "a different binary is a different service",
			unitExe:  "/home/taka/.local/bin/dsh-remote",
			unitArgs: []string{"start", "--lan"},
			args:     []string{"--lan"},
			want:     false,
		},
		{
			name:     "a non-default port can never match a unit",
			unitExe:  self,
			unitArgs: []string{"start"},
			args:     []string{"--dsh-port=3090"},
			want:     false,
		},
		{
			name:     "a unit that does not run start is not ours",
			unitExe:  self,
			unitArgs: []string{"status"},
			args:     []string{"--lan"},
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, set := startFlagsFor(t, tc.args...)
			if got := serviceRunsThisInvocation(tc.unitExe, tc.unitArgs, opts, set); got != tc.want {
				t.Fatalf("serviceRunsThisInvocation(%q, %q, %q) = %t, want %t",
					tc.unitExe, tc.unitArgs, tc.args, got, tc.want)
			}
		})
	}
}

// TestServiceRunsThisInvocationRoundTripsTheInstaller closes the loop with the
// code that writes the unit: whatever install renders for a configuration must
// be recognised as that same configuration when `start` is given the same flags.
func TestServiceRunsThisInvocationRoundTripsTheInstaller(t *testing.T) {
	self := testExecutable(t)
	opts, set := startFlagsFor(t, "--lan", "--service=hermes=127.0.0.1:9119")

	rendered := installStartFlags(opts.lan, opts.lanAddress, opts.noServe, opts.dshExec, opts.trustedHosts, opts.services)
	unitArgs, err := systemd.SplitCommand(rendered)
	if err != nil {
		t.Fatal(err)
	}

	if !serviceRunsThisInvocation(self, append([]string{"start"}, unitArgs...), opts, set) {
		t.Fatalf("install rendered %q, which its own start did not recognise", rendered)
	}
}

// fakeSystemctl writes a stand-in systemctl that records its argv and answers
// `show` with the given properties, so the handoff can be driven without a
// systemd user bus.
func fakeSystemctl(t *testing.T, showLines ...string) (bin string, calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "calls")
	bin = filepath.Join(dir, "systemctl")

	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	script.WriteString("printf '%s\\n' \"$*\" >> " + record + "\n")
	script.WriteString("case \"$*\" in\n  *\" show \"*)\n")
	for _, line := range showLines {
		script.WriteString("    printf '%s\\n' '" + line + "'\n")
	}
	script.WriteString("  ;;\nesac\nexit 0\n")

	if err := os.WriteFile(bin, []byte(script.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() []string {
		data, err := os.ReadFile(record)
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

// writeUnitWithExecStart plants a unit whose ExecStart is exe plus args.
func writeUnitWithExecStart(t *testing.T, exe string, args ...string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "ExecStart=\"" + exe + "\" " + strings.Join(args, " ") + "\n"
	if err := os.WriteFile(filepath.Join(dir, systemd.UnitName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestDelegateToServiceStartsTheMatchingUnit is the shape of the whole handoff:
// a unit that matches this invocation must be started through systemd, with the
// command systemd expects -- not by launching a second proxy.
func TestDelegateToServiceStartsTheMatchingUnit(t *testing.T) {
	self := testExecutable(t)
	bin, calls := fakeSystemctl(t, "ActiveState=inactive", "Restart=on-failure")
	home := writeUnitWithExecStart(t, self, "start", "--lan")

	prevClient, prevWait, prevInside := newSystemdClient, awaitService, insideService
	defer func() { newSystemdClient, awaitService, insideService = prevClient, prevWait, prevInside }()
	newSystemdClient = func() *systemd.Client { return &systemd.Client{Bin: bin, ConfigHome: home} }
	awaitService = func(context.Context, time.Time) error { return nil }
	insideService = func() bool { return false }

	opts, set := startFlagsFor(t, "--lan")
	handled, err := delegateToService(opts, set)
	if !handled {
		t.Fatal("a matching unit was not handed off to")
	}
	if err != nil {
		t.Fatalf("handoff failed: %v", err)
	}
	if got := calls(); !slices.Contains(got, "--user start "+systemd.UnitName) {
		t.Fatalf("systemctl start was not issued; calls = %q", got)
	}
}

// TestDelegateToServiceNeverRecurses guards the unit's own ExecStart: systemd
// runs `dsh-remote start`, so if that process delegated to the unit it would
// either deadlock or exit immediately and leave the service down.
func TestDelegateToServiceNeverRecurses(t *testing.T) {
	self := testExecutable(t)
	bin, calls := fakeSystemctl(t, "ActiveState=active", "Restart=on-failure")
	home := writeUnitWithExecStart(t, self, "start", "--lan")

	prevClient, prevWait, prevInside := newSystemdClient, awaitService, insideService
	defer func() { newSystemdClient, awaitService, insideService = prevClient, prevWait, prevInside }()
	newSystemdClient = func() *systemd.Client { return &systemd.Client{Bin: bin, ConfigHome: home} }
	awaitService = func(context.Context, time.Time) error { return nil }
	insideService = func() bool { return true } // we ARE the service

	opts, set := startFlagsFor(t)
	handled, err := delegateToService(opts, set)
	if handled || err != nil {
		t.Fatalf("delegateToService(inside the service) = (%t, %v), want (false, nil)", handled, err)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("the service delegated to itself; systemctl calls = %q", got)
	}
}

// TestDelegateToServiceRefusesAMismatch keeps a mismatched invocation from being
// quietly started as the unit's configuration: it must report what the unit runs
// and leave systemd alone.
func TestDelegateToServiceRefusesAMismatch(t *testing.T) {
	self := testExecutable(t)
	bin, calls := fakeSystemctl(t, "ActiveState=inactive", "Restart=on-failure")
	home := writeUnitWithExecStart(t, self, "start", "--lan")

	prevClient, prevWait, prevInside := newSystemdClient, awaitService, insideService
	defer func() { newSystemdClient, awaitService, insideService = prevClient, prevWait, prevInside }()
	newSystemdClient = func() *systemd.Client { return &systemd.Client{Bin: bin, ConfigHome: home} }
	awaitService = func(context.Context, time.Time) error { return nil }
	insideService = func() bool { return false }

	opts, set := startFlagsFor(t, "--no-serve")
	handled, err := delegateToService(opts, set)
	if !handled || err == nil {
		t.Fatalf("mismatched invocation = (%t, %v), want handled with an error", handled, err)
	}
	if got := calls(); slices.Contains(got, "--user start "+systemd.UnitName) {
		t.Fatalf("systemctl start was issued for a mismatched invocation: %q", got)
	}
}

// TestDelegateStopStopsTheActiveUnit mirrors the start side: when the unit is
// what is running, stop it through systemd rather than signalling a pid out from
// under it.
func TestDelegateStopStopsTheActiveUnit(t *testing.T) {
	bin, calls := fakeSystemctl(t, "ActiveState=active", "Restart=on-failure")
	home := writeUnitWithExecStart(t, "/bin/true", "start")

	prev := newSystemdClient
	defer func() { newSystemdClient = prev }()
	newSystemdClient = func() *systemd.Client { return &systemd.Client{Bin: bin, ConfigHome: home} }

	handled, err := delegateStop(defaultProxyPort)
	if !handled || err != nil {
		t.Fatalf("delegateStop = (%t, %v), want handled with no error", handled, err)
	}
	if got := calls(); !slices.Contains(got, "--user stop "+systemd.UnitName) {
		t.Fatalf("systemctl stop was not issued; calls = %q", got)
	}

	// A non-default port is not this service, so it must not touch the unit.
	if handled, _ := delegateStop(9999); handled {
		t.Fatal("delegateStop claimed a non-default proxy port belonged to the unit")
	}
}

// TestRunningAsServiceIsDecidedByCgroupNotEnvironment pins the guard that keeps
// the unit's own `start` from delegating to itself. The invocation id is
// inherited by every descendant -- DSH runs agent commands in systemd scopes --
// so trusting it alone would make an agent-issued `start` claim to be the
// service. A cgroup view that names no unit (a cgroup namespace) must fall back
// to the invocation id rather than conclude "not the service", because the
// opposite mistake makes the unit never start.
func TestRunningAsServiceIsDecidedByCgroupNotEnvironment(t *testing.T) {
	const (
		unitCgroup  = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/dsh-remote.service\n"
		scopeCgroup = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/dsh-subprocess-123-a1b2.scope\n"
		hiddenView  = "0::/\n"
	)

	cases := []struct {
		name       string
		cgroup     string
		unreadable bool
		invocation string
		want       bool
	}{
		{"the unit's own process", unitCgroup, false, "abc", true},
		{"a DSH agent scope that inherited the invocation id", scopeCgroup, false, "abc", false},
		{"a cgroup namespace hides the unit name", hiddenView, false, "abc", true},
		{"hidden view and no invocation id", hiddenView, false, "", false},
		{"unreadable cgroup falls back to the invocation id", "", true, "abc", true},
		{"unreadable cgroup and none set", "", true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev := cgroupData
			t.Cleanup(func() { cgroupData = prev })
			if tc.unreadable {
				cgroupData = func() ([]byte, error) { return nil, os.ErrNotExist }
			} else {
				cgroupData = func() ([]byte, error) { return []byte(tc.cgroup), nil }
			}
			t.Setenv("INVOCATION_ID", tc.invocation)

			if got := runningAsService(); got != tc.want {
				t.Fatalf("runningAsService() = %t, want %t (cgroup %q, INVOCATION_ID %q)",
					got, tc.want, tc.cgroup, tc.invocation)
			}
		})
	}
}

// TestClassifyMismatchSeparatesTheTwoCases pins the distinction that keeps the
// refusal honest. "Same flags, different binary" is the ordinary post-rebuild
// case; reporting it as a configuration difference, while printing two identical
// flag lists, sends the operator looking for a problem that does not exist.
func TestClassifyMismatchSeparatesTheTwoCases(t *testing.T) {
	self := testExecutable(t)
	const otherBin = "/home/taka/.local/bin/dsh-remote"

	cases := []struct {
		name     string
		unitExe  string
		unitArgs []string
		args     []string
		want     mismatchKind
	}{
		{
			name:     "same flags, different build",
			unitExe:  otherBin,
			unitArgs: []string{"start", "--lan"},
			args:     []string{"--lan"},
			want:     mismatchBuild,
		},
		{
			name:     "different flags",
			unitExe:  self,
			unitArgs: []string{"start", "--lan"},
			args:     []string{"--no-serve"},
			want:     mismatchConfig,
		},
		{
			name:     "different port is a configuration difference",
			unitExe:  otherBin,
			unitArgs: []string{"start", "--lan"},
			args:     []string{"--lan", "--dsh-port=3090"},
			want:     mismatchConfig,
		},
		{
			name:     "the unit runs no start at all",
			unitExe:  otherBin,
			unitArgs: []string{"status"},
			args:     []string{"--lan"},
			want:     mismatchConfig,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, _ := startFlagsFor(t, tc.args...)
			if got := classifyMismatch(tc.unitExe, tc.unitArgs, opts); got != tc.want {
				t.Fatalf("classifyMismatch(%q, %q, %q) = %v, want %v",
					tc.unitExe, tc.unitArgs, tc.args, got, tc.want)
			}
		})
	}
}
