package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/qr"
	"github.com/tkoizumi/dsh-remote/internal/systemd"
)

// serviceHandoffTimeout bounds how long a delegated `systemctl start` is given
// to produce a proxy that answers.
const serviceHandoffTimeout = 90 * time.Second

// serviceFlagsSet returns the `start` flags whose value differs from the
// default. Those are the flags that decide what the installed unit would run, so
// setting one means the operator is asking for a configuration rather than just
// "start the service". Passing a flag at its default value (`--dsh-port 3080`)
// is not a request for anything different.
func serviceFlagsSet(fs *flag.FlagSet, opts startOptions) []string {
	var set []string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "dsh-port":
			if opts.dshPort != defaultDSHPort {
				set = append(set, f.Name)
			}
		case "proxy-port":
			if opts.proxyPort != defaultProxyPort {
				set = append(set, f.Name)
			}
		case "lan", "lan-address", "no-serve", "dsh-exec", "trusted-host", "service":
			set = append(set, f.Name)
		}
	})
	return set
}

// Overridable seams. The handoff orchestration needs a systemd user bus and a
// service that takes seconds to come up; tests substitute both so the decision
// and the command it issues can be checked directly.
var (
	newSystemdClient = systemd.New
	awaitService     = waitForServiceReady
	insideService    = runningAsService
)

// cgroupData reads this process's cgroup membership. Overridable so the
// membership decision can be tested for both the normal view and a cgroup
// namespace that hides the unit name.
var cgroupData = func() ([]byte, error) { return os.ReadFile("/proc/self/cgroup") }

// runningAsService reports whether this process is the systemd unit's own main
// process, rather than something that merely inherited systemd's environment.
//
// INVOCATION_ID alone is not enough. systemd exports it to every descendant, and
// DSH runs agent commands under its own systemd-run scopes -- so a
// `dsh-remote start` issued from an agent session inherits an invocation id,
// would claim to be the service, and would skip the handoff to start a competing
// foreground proxy: the exact race the handoff exists to prevent. The cgroup does
// not lie. The service's own process is in the unit's cgroup; anything systemd
// started separately, including a task in another scope, is not.
//
// A cgroup view can be made uninformative -- ProtectControlGroups=private or
// strict hides everything but "/" -- so a view that names no unit at all falls
// back to the invocation id rather than concluding "not the service". Trusting
// it there is the safe direction: assuming "we are the service" avoids launching
// a second proxy under a unit, whereas the opposite would make the unit delegate
// to itself and never come up.
func runningAsService() bool {
	if data, err := cgroupData(); err == nil {
		text := string(data)
		if strings.Contains(text, ".service") || strings.Contains(text, ".scope") {
			return strings.Contains(text, systemd.UnitName)
		}
	}
	return os.Getenv("INVOCATION_ID") != ""
}

// delegateToService hands `start` to the installed systemd unit when one owns
// this deployment.
//
// It returns handled=true when the request was answered by the service, which
// includes refusing to start a second, unmanaged proxy beside it. That refusal
// is the point: a foreground proxy holding the ports while an enabled unit
// retries them forever is the configuration behind the restart storm this tool
// exists to avoid, and it should not be reachable by typing the obvious command.
func delegateToService(opts startOptions, setFlags []string) (bool, error) {
	// Never when systemd launched this process: the unit's ExecStart is
	// `dsh-remote start`, so delegating to it would recurse.
	if insideService() {
		return false, nil
	}

	ctx := context.Background()
	client := newSystemdClient()
	sup := client.Query(ctx)
	if !sup.Supported || !sup.Installed {
		return false, nil // nothing installed: run in this terminal, as before
	}

	// Decide whether the unit is this invocation before asking the bus anything:
	// a configuration mismatch is a user error, and reporting it does not need
	// systemd to be reachable.
	unitExe, unitArgs, err := client.ExecStart()
	if err != nil {
		return true, fmt.Errorf("read the installed service: %w", err)
	}
	if !serviceRunsThisInvocation(unitExe, unitArgs, opts, setFlags) {
		describeServiceMismatch(unitExe, unitArgs, opts)
		return true, errors.New("refusing to start a second, unmanaged proxy beside the installed service")
	}

	if sup.Err != nil {
		return true, fmt.Errorf("a unit is installed at %s but the systemd user bus could not be queried (%v); refusing to start a second, unmanaged proxy -- fix the bus, or run in this terminal with --foreground", sup.UnitPath, sup.Err)
	}

	if sup.Active {
		fmt.Printf("dsh-remote is already running under systemd (%s).\n", systemd.UnitName)
		reportService(opts)
		return true, nil
	}

	requested := time.Now()
	if err := client.Start(ctx); err != nil {
		return true, err
	}
	if err := awaitService(ctx, requested); err != nil {
		return true, fmt.Errorf("%v; check `journalctl --user -u %s`", err, systemd.UnitName)
	}
	fmt.Printf("dsh-remote is running under systemd (%s).\n", systemd.UnitName)
	reportService(opts)
	return true, nil
}

// serviceRunsThisInvocation reports whether the unit's ExecStart is the same
// service this command line asked for.
func serviceRunsThisInvocation(unitExe string, unitArgs []string, opts startOptions, setFlags []string) bool {
	// A bare `dsh-remote start` means "start the service as installed".
	if len(setFlags) == 0 {
		return true
	}
	// Ports are the one thing a unit cannot express, so a non-default port can
	// never match it.
	if opts.dshPort != defaultDSHPort || opts.proxyPort != defaultProxyPort {
		return false
	}
	// The binary matters as much as the flags: after a rebuild, delegating to a
	// unit that points at an older installed build would quietly run the wrong
	// code -- the opposite of what someone who just built this expects.
	if !sameExecutable(unitExe) {
		return false
	}
	return sameServiceFlags(unitArgs, opts)
}

// sameServiceFlags reports whether the unit's argument vector is the flags this
// invocation asked for.
func sameServiceFlags(unitArgs []string, opts startOptions) bool {
	if len(unitArgs) == 0 || unitArgs[0] != "start" {
		return false
	}
	want, err := systemd.SplitCommand(strings.TrimSpace(installStartFlags(
		opts.lan, opts.lanAddress, opts.noServe, opts.dshExec, opts.trustedHosts, opts.services)))
	if err != nil {
		return false
	}
	return slices.Equal(want, unitArgs[1:])
}

// selfExecutable returns the running binary's path with symlinks resolved, the
// same way `install` does when it writes ExecStart.
func selfExecutable() (string, bool) {
	self, err := os.Executable()
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return self, true
}

// sameExecutable reports whether path names the running dsh-remote.
func sameExecutable(path string) bool {
	self, ok := selfExecutable()
	if !ok {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(self) == filepath.Clean(path)
}

// mismatchKind says why the installed unit is not this invocation, so the
// explanation can be specific. "Same flags, different binary" is the ordinary
// case after a rebuild, and it needs a different instruction from a genuine
// configuration difference -- saying "different configuration" while printing
// two identical flag lists is how an operator ends up hunting a problem that is
// not there.
type mismatchKind int

const (
	mismatchConfig mismatchKind = iota
	mismatchBuild
)

// classifyMismatch distinguishes the two cases.
func classifyMismatch(unitExe string, unitArgs []string, opts startOptions) mismatchKind {
	portsDefault := opts.dshPort == defaultDSHPort && opts.proxyPort == defaultProxyPort
	if portsDefault && sameServiceFlags(unitArgs, opts) && !sameExecutable(unitExe) {
		return mismatchBuild
	}
	return mismatchConfig
}

// describeServiceMismatch explains what the installed service runs, and the ways
// to proceed, rather than leaving the operator to guess.
func describeServiceMismatch(unitExe string, unitArgs []string, opts startOptions) {
	if classifyMismatch(unitExe, unitArgs, opts) == mismatchBuild {
		self, _ := selfExecutable()
		fmt.Fprintln(os.Stderr, "the installed service runs a different build of dsh-remote:")
		fmt.Fprintf(os.Stderr, "  service: %s\n", unitExe)
		fmt.Fprintf(os.Stderr, "  you ran: %s\n", self)
		fmt.Fprintf(os.Stderr, "  install this build: install -m755 %s %s.new && mv -f %s.new %s\n",
			self, unitExe, unitExe, unitExe)
		fmt.Fprintln(os.Stderr, "  run here anyway:    dsh-remote start --foreground")
		return
	}
	fmt.Fprintf(os.Stderr, "the installed service (%s) runs a different configuration:\n", systemd.UnitName)
	fmt.Fprintf(os.Stderr, "  service: %s\n", strings.Join(append([]string{unitExe}, unitArgs...), " "))
	fmt.Fprintf(os.Stderr, "  you ran: %s\n", strings.Join(os.Args, " "))
	fmt.Fprintln(os.Stderr, "  adopt your flags: dsh-remote install --force")
	fmt.Fprintln(os.Stderr, "  run here anyway:  dsh-remote start --foreground")
}

// reportService prints the summary a foreground start would have printed, read
// from the run state the service wrote.
func reportService(opts startOptions) {
	state, err := process.LoadState()
	if err != nil || state == nil {
		fmt.Fprintf(os.Stderr, "note: no run state is readable yet; try `dsh-remote status` or `journalctl --user -u %s`\n", systemd.UnitName)
		return
	}
	printStartSummary(opts, state.TailnetHost, state.LANAddr, state.DSHPID, state.Services)
	fmt.Fprintf(os.Stderr, "Logs: journalctl --user -u %s -f\n", systemd.UnitName)

	qrURL := state.RemoteURL
	if qrURL == "" {
		qrURL = state.LANURL
	}
	if !opts.noQR && qrURL != "" {
		fmt.Println("Scan this QR code from your phone:")
		if err := qr.Render(os.Stdout, qrURL); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
	}
}

// waitForServiceReady blocks until the freshly started service has written run
// state and its proxy answers, so a delegated start reports success only when
// there is something to connect to.
func waitForServiceReady(ctx context.Context, since time.Time) error {
	deadline := time.Now().Add(serviceHandoffTimeout)
	for {
		if state, err := process.LoadState(); err == nil && state != nil && state.StartedAt.After(since) {
			if state.ProxyAlive() {
				if _, ok := fetchHealth(state.ProxyAddr); ok {
					return nil
				}
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the service did not become ready within %s", serviceHandoffTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// delegateStop hands `stop` to the installed unit when it is what is running, so
// the service stops the way systemd expects instead of being signalled out from
// under it. handled is true only when the unit was found active.
func delegateStop(proxyPort int) (bool, error) {
	if proxyPort != defaultProxyPort {
		return false, nil
	}
	ctx := context.Background()
	client := newSystemdClient()
	sup := client.Query(ctx)
	if !sup.Supported || !sup.Installed || !sup.Active || sup.Err != nil {
		return false, nil
	}
	if err := client.Stop(ctx); err != nil {
		return true, err
	}
	return true, nil
}
