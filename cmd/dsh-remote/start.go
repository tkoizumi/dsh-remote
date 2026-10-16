package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/dsh"
	"github.com/tkoizumi/dsh-remote/internal/logging"
	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/proxy"
	"github.com/tkoizumi/dsh-remote/internal/qr"
	"github.com/tkoizumi/dsh-remote/internal/systemd"
	"github.com/tkoizumi/dsh-remote/internal/tailscale"
)

// Default loopback ports. Both services bind 127.0.0.1 only.
const (
	defaultDSHPort   = 3080
	defaultProxyPort = 3081
)

type startOptions struct {
	dshPort      int
	proxyPort    int
	npx          string
	dshExec      string
	tailnetHost  string
	trustedHosts stringList
	services     stringList
	lan          bool
	lanAddress   string
	noServe      bool
	noQR         bool
	foreground   bool
	tokenTimeout time.Duration
}

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	if value == "" {
		return errors.New("value must not be empty")
	}
	*s = append(*s, value)
	return nil
}

// registerStartFlags defines the `start` flag set. It is separate from runStart
// so tests can parse the real defaults -- the service handoff decision turns on
// which flags were actually set, and a hand-rolled copy would drift.
func registerStartFlags(fs *flag.FlagSet, opts *startOptions) {
	fs.IntVar(&opts.dshPort, "dsh-port", defaultDSHPort, "loopback port for DeepSeek Harness")
	fs.IntVar(&opts.proxyPort, "proxy-port", defaultProxyPort, "loopback port for the stable proxy")
	fs.StringVar(&opts.npx, "npx", "npx", "npx executable to launch DeepSeek Harness with")
	fs.StringVar(&opts.dshExec, "dsh-exec", "", "run this dsh executable directly instead of via npx")
	fs.StringVar(&opts.tailnetHost, "tailnet-host", "", "override the detected Tailscale hostname")
	fs.Var(&opts.trustedHosts, "trusted-host", "extra authority to pass to dsh --trusted-host (repeatable)")
	fs.Var(&opts.services, "service", "also expose a loopback dashboard (repeatable): name=host:port, for example hermes=127.0.0.1:9119")
	fs.BoolVar(&opts.lan, "lan", false, "also serve the stable URL on this host's local network address (plain HTTP)")
	fs.StringVar(&opts.lanAddress, "lan-address", "", "local network address to bind for --lan (default: detected)")
	fs.BoolVar(&opts.noServe, "no-serve", false, "do not touch the Tailscale Serve configuration")
	fs.BoolVar(&opts.noQR, "no-qr", false, "do not print a QR code")
	fs.BoolVar(&opts.foreground, "foreground", false, "run in this terminal instead of handing off to the installed systemd service")
	fs.DurationVar(&opts.tokenTimeout, "token-timeout", 90*time.Second, "how long to wait for DeepSeek Harness to print its token URL")
}

// runStart is declared with a named error result so the deferred teardown can
// record why the run ended. Without it, a preflight refusal or a bind failure
// was logged as "shutdown: unknown", which reads like an unexplained crash --
// exactly what the persistent log exists to rule out.
func runStart(args []string) (err error) {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	var opts startOptions
	registerStartFlags(fs, &opts)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: dsh-remote start [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Validate the extra dashboards before anything is launched or written.
	specs, err := parseServiceSpecs(opts.services)
	if err != nil {
		return err
	}
	if len(specs) > 0 && opts.noServe && !opts.lan {
		return errors.New("--service needs somewhere to serve: drop --no-serve (for the tailnet URL) or add --lan (for the local network URL)")
	}

	// Once a unit is installed it owns this deployment, so `start` drives it
	// rather than racing it with a second, unmanaged proxy. --foreground keeps
	// the in-terminal behaviour for debugging.
	if !opts.foreground {
		if handled, err := delegateToService(opts, serviceFlagsSet(fs, opts)); handled {
			return err
		}
	}

	existing, err := process.LoadState()
	if err != nil {
		return err
	}

	// --- Persistent log ----------------------------------------------------
	// Opened before anything can fail, so the port preflight, the launch, and
	// every early error path leave a record that survives the terminal, the
	// process, and a reboot.
	procOut := io.Writer(os.Stderr)
	var log *logging.Logger
	if path, logErr := logging.Path(); logErr == nil {
		log = logging.Open(path, os.Stderr)
		if log.Enabled() {
			log.Logf("dsh-remote %s starting: dsh port %d, proxy port %d, serve %s",
				version, opts.dshPort, opts.proxyPort, serveMode(opts.noServe))
			log.Logf("log file: %s", log.Path())
			// Every log line is redacted, so relaying DeepSeek Harness output
			// into it cannot persist the credential.
			procOut = io.MultiWriter(log, os.Stderr)
		}
	}
	if existing != nil && existing.ProxyAlive() {
		if log != nil {
			log.Logf("refusing to start: recorded proxy pid %d is still alive", existing.PID)
			_ = log.Close()
		}
		return fmt.Errorf("dsh-remote is already running (pid %d); run `dsh-remote stop` first", existing.PID)
	}
	if existing != nil && existing.PID > 0 {
		// The recorded run is provably not the process that wrote it -- a
		// leftover from a previous boot, or the pid has been reused. Saying so
		// beats silently ignoring state that a human may be looking at.
		if log != nil {
			log.Logf("state: recorded proxy pid %d is not the process that wrote this state (stale); ignoring it", existing.PID)
		}
	}
	warnIfUnmanagedNextToService(log)
	warnIfTTYWithoutSupervisor(log)

	var (
		ts              *tailscale.Client
		proc            *dsh.Process
		server          *proxy.Server
		services        []*runningService
		priorTarget     string
		serveConfigured bool
		stateSaved      bool
		exitReason      string
	)
	var teardownOnce sync.Once
	teardown := func() {
		teardownOnce.Do(func() {
			if log != nil {
				log.Logf("shutdown: %s", describeExit(exitReason, err))
			}
			if server != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = server.Shutdown(shutdownCtx)
				cancel()
				if log != nil {
					log.Logf("proxy: stopped")
				}
			}
			for _, rs := range services {
				if rs.forwarder == nil {
					continue
				}
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = rs.forwarder.Shutdown(shutdownCtx)
				cancel()
				if log != nil {
					log.Logf("service %s: stopped", rs.spec.Name)
				}
			}
			if proc != nil {
				_ = proc.Stop(5 * time.Second)
				if log != nil {
					log.Logf("dsh: stopped (pid %d)", proc.PID())
				}
			}
			if serveConfigured {
				restoreServe(ts, priorTarget, opts.proxyPort, log)
			}
			for _, rs := range services {
				if rs.serveSet && ts != nil {
					restoreServeSlot(ts, rs.httpsPort, servicePath, rs.priorTarget, log)
				}
			}
			if stateSaved {
				if err := process.RemoveState(); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not remove state file: %v\n", err)
				} else if log != nil {
					log.Logf("state: removed")
				}
			}
			if log != nil {
				log.Logf("shutdown complete")
				_ = log.Close()
			}
		})
	}
	defer teardown()

	// Reclaim the DeepSeek Harness port before anything else. When the proxy is
	// killed without running its cleanup, the child it launched keeps the port
	// in its own process group; without this, every supervised restart would
	// fail on the same port conflict and the outage would outlive the process
	// that caused it. An unrelated `dsh web` is left strictly alone.
	preflight, err := dshPortPreflight(opts.dshPort, existing, func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "dsh-remote: "+format+"\n", args...)
		if log != nil {
			log.Logf("preflight: "+format, args...)
		}
	})
	if err != nil {
		if log != nil {
			log.Logf("preflight refused: %v", err)
		}
		return err
	}
	if preflight == portReclaimed {
		// The stale run state was removed with the orphan, so nothing below
		// mistakes the dead proxy for a live one.
		existing = nil
	}

	// --- Prerequisites -----------------------------------------------------
	ctx := context.Background()
	spec := dsh.Spec{NPX: opts.npx, Port: opts.dshPort, Package: dsh.DefaultPackage}

	if opts.dshExec != "" {
		execPath, err := dsh.LookupExec(opts.dshExec)
		if err != nil {
			return err
		}
		spec.Exec = execPath
	} else {
		npxPath, err := dsh.LookupNpx(opts.npx)
		if err != nil {
			return err
		}
		spec.NPX = npxPath
	}

	if !opts.noServe {
		var err error
		ts, err = tailscale.New()
		if err != nil {
			return err
		}
	}

	if err := process.PortAvailable(dsh.Loopback, opts.dshPort); err != nil {
		// dshPortPreflight already reclaimed or refused an attributable holder,
		// so reaching here means the port is held by something else entirely.
		if log != nil {
			log.Logf("dsh port unavailable: %v", err)
		}
		return fmt.Errorf("%w (held by a process dsh-remote did not launch; free it or choose another port with --dsh-port)", err)
	}
	if err := process.PortAvailable(dsh.Loopback, opts.proxyPort); err != nil {
		if log != nil {
			log.Logf("proxy port unavailable: %v", err)
		}
		return err
	}

	// --- Optional local network address ------------------------------------
	// A MacBook on the same WiFi can use the stable URL without Tailscale, at
	// the cost of exposing it to that network over plain HTTP.
	lanAddr := ""
	if opts.lan {
		lanAddr = opts.lanAddress
		if lanAddr == "" {
			addresses := process.LANAddresses()
			if len(addresses) == 0 {
				return errors.New("--lan: no local network address found; pass --lan-address")
			}
			// LANAddresses orders by the host's default route first, so the
			// address other machines can actually reach wins over a lexically
			// smaller Docker bridge or veth.
			lanAddr = addresses[0]
			if len(addresses) > 1 {
				fmt.Fprintf(os.Stderr, "note: local network addresses %v; using %s from the default route (override with --lan-address)\n", addresses, lanAddr)
			}
		} else if err := process.ValidateLANAddress(lanAddr); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "warning: --lan publishes %s over plain HTTP to the local network; "+
			"anyone on it can open %s and obtain a DeepSeek Harness session\n", lanAddr, proxy.BootstrapPath)
		if err := process.PortAvailable(lanAddr, opts.proxyPort); err != nil {
			return err
		}
	}

	// --- Extra dashboards --------------------------------------------------
	// Each --service gets a loopback reverse proxy and, when Serve is managed, a
	// dedicated tailnet HTTPS port. Its LAN listener is opt-in with --lan for the
	// same reason DSH's is: that path is plain HTTP with no authentication.
	for i, spec := range specs {
		if spec.Port == opts.dshPort || spec.Port == opts.proxyPort {
			return fmt.Errorf("--service %s: port %d is already used by dsh-remote itself", spec.Name, spec.Port)
		}
		upstream, err := url.Parse(spec.target())
		if err != nil {
			return fmt.Errorf("--service %s: %w", spec.Name, err)
		}
		rs := &runningService{
			spec:      spec,
			httpsPort: serviceServePorts[i],
			forwarder: proxy.NewForwarder(upstream),
		}
		if opts.lan {
			if err := process.PortAvailable(lanAddr, spec.Port); err != nil {
				return fmt.Errorf("--service %s: %w", spec.Name, err)
			}
			fmt.Fprintf(os.Stderr, "warning: --lan publishes service %s over plain HTTP at http://%s; "+
				"anyone on the local network can open it\n", spec.Name, net.JoinHostPort(lanAddr, strconv.Itoa(spec.Port)))
		}
		services = append(services, rs)
	}

	// --- Tailnet identity --------------------------------------------------
	tailnetHost := opts.tailnetHost
	if !opts.noServe && tailnetHost == "" {
		host, err := ts.DNSName(ctx)
		if err != nil {
			return err
		}
		tailnetHost = host
	}
	// DeepSeek Harness's /api fence only accepts loopback or explicitly trusted
	// authorities. The phone arrives under the tailnet name and a browser on the
	// local network under the LAN address, so both must be declared.
	if tailnetHost != "" {
		spec.TrustedHosts = append(spec.TrustedHosts, tailnetHost)
	}
	if lanAddr != "" {
		spec.TrustedHosts = append(spec.TrustedHosts, lanAddr)
	}
	spec.TrustedHosts = append(spec.TrustedHosts, opts.trustedHosts...)

	target := "http://" + dsh.Loopback + ":" + strconv.Itoa(opts.proxyPort)

	// --- Tailscale Serve preflight -----------------------------------------
	// Configure Serve before launching anything. A privilege problem then fails
	// fast, with the chance to fix it in place, instead of after DeepSeek
	// Harness is already running.
	if !opts.noServe {
		var err error
		priorTarget, err = ts.RootTarget(ctx)
		if err != nil {
			return err
		}
		if priorTarget != "" && priorTarget != target {
			fmt.Fprintf(os.Stderr, "warning: tailscale serve already maps / to %s; replacing it for now and restoring it on stop\n", priorTarget)
		}
		// Probe with the mapping that is already there when there is one, so the
		// probe cannot break a working URL. When the root is unclaimed, probing
		// with our own target changes nothing that worked before.
		probe := target
		if priorTarget != "" {
			probe = priorTarget
		}
		if err := ensureServePermission(ctx, ts, probe); err != nil {
			return err
		}
		if priorTarget == "" {
			// The probe already installed our handler.
			serveConfigured = true
		}
	}

	// --- Launch DeepSeek Harness ------------------------------------------
	if log != nil {
		log.Logf("dsh: launching %s", describeSpec(spec))
	}
	fmt.Fprintln(os.Stderr, "Starting DeepSeek Harness...")
	proc, err = dsh.Start(ctx, spec, procOut)
	if err != nil {
		if log != nil {
			log.Logf("dsh failed to start: %v", err)
		}
		return err
	}
	if log != nil {
		log.Logf("dsh: started with pid %d", proc.PID())
	}

	if _, err := proc.WaitToken(opts.tokenTimeout); err != nil {
		if log != nil {
			log.Logf("dsh: no startup token: %v", err)
		}
		return err
	}
	if log != nil {
		// Whether the token exists is logged; its value never is.
		log.Logf("dsh: startup token captured (value withheld)")
	}

	// --- Stable proxy ------------------------------------------------------
	upstream, err := url.Parse("http://" + dsh.Loopback + ":" + strconv.Itoa(opts.dshPort))
	if err != nil {
		return err
	}
	server = proxy.New(upstream, func() string { return proc.Token().Token }, version)

	var listeners []net.Listener
	loopbackListener, err := proxy.Listen(opts.proxyPort)
	if err != nil {
		return err
	}
	listeners = append(listeners, loopbackListener)
	if lanAddr != "" {
		lanListener, err := proxy.ListenOn(lanAddr, opts.proxyPort)
		if err != nil {
			return fmt.Errorf("listen on the local network address: %w", err)
		}
		// Binding the address is not enough: an override that resolves to
		// loopback (or a listener that silently landed on it) would advertise a
		// "local network URL" that no other machine can open. Refuse that
		// outright, then confirm with a self-dial that the address accepts
		// connections at all.
		if err := verifyLANListener(lanListener, lanAddr, opts.proxyPort, log); err != nil {
			_ = lanListener.Close()
			return err
		}
		listeners = append(listeners, lanListener)
	}
	serveErr := make(chan error, len(listeners)+len(services))
	for _, listener := range listeners {
		if log != nil {
			log.Logf("proxy: listening on %s", listener.Addr())
		}
		go func() { serveErr <- server.Serve(listener) }()
	}
	for _, rs := range services {
		if !opts.lan {
			continue
		}
		ln, err := proxy.ListenOn(lanAddr, rs.spec.Port)
		if err != nil {
			return fmt.Errorf("service %s: listen on %s: %w", rs.spec.Name, lanAddr, err)
		}
		// Same guard as the DSH LAN listener: never advertise a service URL
		// that only loopback can open.
		if err := verifyLANListener(ln, lanAddr, rs.spec.Port, log); err != nil {
			_ = ln.Close()
			return fmt.Errorf("service %s: %w", rs.spec.Name, err)
		}
		rs.listener = ln
		if log != nil {
			log.Logf("service %s: listening on %s", rs.spec.Name, ln.Addr())
		}
		go func(rs *runningService) { serveErr <- rs.forwarder.Serve(rs.listener) }(rs)
	}

	// Point Serve at the proxy only now that it is listening, so replacing an
	// existing mapping does not open a window of 502s for the current URL.
	if !opts.noServe && !serveConfigured {
		if err := ts.SetRootProxy(ctx, target); err != nil {
			if log != nil {
				log.Logf("tailscale serve failed: %v", err)
			}
			return fmt.Errorf("configure tailscale serve: %w", err)
		}
		serveConfigured = true
		if log != nil {
			log.Logf("tailscale: serve / -> %s", target)
		}
	}
	// Expose each extra dashboard on its own HTTPS port, at its own root so its
	// absolute asset URLs keep working. The prior slot is recorded so stop can
	// put back whatever was there before.
	if !opts.noServe {
		for _, rs := range services {
			prior, err := ts.ServeTarget(ctx, rs.httpsPort, servicePath)
			if err != nil {
				return fmt.Errorf("service %s: read tailscale serve: %w", rs.spec.Name, err)
			}
			rs.priorTarget = prior
			if prior != "" && prior != rs.forwarder.Upstream() {
				fmt.Fprintf(os.Stderr, "warning: tailscale serve already maps / on port %d to %s; replacing it for now and restoring it on stop\n", rs.httpsPort, prior)
			}
			if err := ts.SetServeProxy(ctx, rs.httpsPort, servicePath, rs.forwarder.Upstream()); err != nil {
				return fmt.Errorf("service %s: configure tailscale serve on port %d: %w", rs.spec.Name, rs.httpsPort, err)
			}
			rs.serveSet = true
			if log != nil {
				log.Logf("tailscale: serve %s on port %d -> %s", servicePath, rs.httpsPort, rs.forwarder.Upstream())
			}
		}
	}
	if log != nil {
		if opts.noServe {
			log.Logf("tailscale: serve not managed (--no-serve)")
		} else if priorTarget != "" && priorTarget != target {
			log.Logf("tailscale: replaced existing serve / -> %s (will restore on stop)", priorTarget)
		}
	}

	// --- Persist run state (never the token) -------------------------------
	state := &process.State{
		PID:              os.Getpid(),
		DSHPID:           proc.PID(),
		DSHAddr:          "http://" + dsh.Loopback + ":" + strconv.Itoa(opts.dshPort),
		ProxyAddr:        "http://" + dsh.Loopback + ":" + strconv.Itoa(opts.proxyPort),
		TailnetHost:      tailnetHost,
		LANAddr:          lanAddr,
		PriorServeTarget: priorTarget,
		ServeConfigured:  serveConfigured,
		Version:          version,
		StartedAt:        time.Now(),
		// Record identity that survives a reboot and cannot be reused: a bare
		// pid is not enough to recognise this run later.
		BootID:     process.BootID(),
		ProxyStart: process.StartTime(os.Getpid()),
		DSHStart:   process.StartTime(proc.PID()),
	}
	if tailnetHost != "" {
		state.RemoteURL = "https://" + tailnetHost + proxy.BootstrapPath
	}
	if lanAddr != "" {
		state.LANURL = "http://" + net.JoinHostPort(lanAddr, strconv.Itoa(opts.proxyPort)) + proxy.BootstrapPath
	}
	for _, rs := range services {
		svc := process.ServiceState{
			Name:             rs.spec.Name,
			Target:           rs.forwarder.Upstream(),
			HTTPSPort:        rs.httpsPort,
			Path:             servicePath,
			PriorServeTarget: rs.priorTarget,
			ServeConfigured:  rs.serveSet,
		}
		if rs.listener != nil {
			svc.LANAddr = lanAddr
			svc.LANURL = "http://" + net.JoinHostPort(lanAddr, strconv.Itoa(rs.spec.Port)) + servicePath
		}
		if tailnetHost != "" && !opts.noServe {
			svc.TailnetURL = "https://" + net.JoinHostPort(tailnetHost, strconv.Itoa(rs.httpsPort)) + servicePath
		}
		state.Services = append(state.Services, svc)
	}
	if err := process.SaveState(state); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		if log != nil {
			log.Logf("state: could not be saved: %v", err)
		}
	} else {
		stateSaved = true
		if log != nil {
			log.Logf("state: saved (proxy pid %d, dsh pid %d)", state.PID, state.DSHPID)
		}
	}

	// --- Announce ----------------------------------------------------------
	printStartSummary(opts, tailnetHost, lanAddr, proc.PID(), state.Services)
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
	fmt.Fprintln(os.Stderr, "\nPress Ctrl+C to stop. DeepSeek Harness output follows.")
	if log != nil {
		log.Logf("ready: proxy on %s, dsh on %s", state.ProxyAddr, state.DSHAddr)
	}

	// --- Supervise ---------------------------------------------------------
	//
	// The signal set is deliberate. SIGINT and SIGTERM are explicit stop
	// requests, so they shut down cleanly and are recorded. SIGHUP is not a stop
	// request: it means the terminal or session went away, and a service whose
	// whole job is keeping a URL reachable has to outlive that rather than be
	// killed by it. Dying on hangup is what left a stale state file and an
	// orphaned DeepSeek Harness holding the port; this is the guarantee `nohup`
	// gives, applied to the one process that cannot rely on the operator
	// remembering to ask for it.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	// A vanished output sink must not be fatal either. Writing to a closed pipe
	// on stdout or stderr would otherwise raise a fatal SIGPIPE and end the run
	// with no teardown and no log line -- the same silent-death shape as the
	// hangup. Notify is used rather than Ignore on purpose: it installs a
	// handler, which is reset to the default across exec, so the DeepSeek
	// Harness child keeps its own SIGPIPE disposition. The channel is never
	// drained because the signal only has to stop being fatal.
	sigPipe := make(chan os.Signal, 1)
	signal.Notify(sigPipe, syscall.SIGPIPE)
	defer signal.Stop(sigPipe)

	var hangupNotice sync.Once
	for {
		select {
		case sig := <-sigCh:
			if actionFor(sig) == signalContinue {
				hangupNotice.Do(func() {
					const notice = "the controlling terminal went away; continuing to serve (SIGHUP ignored)"
					fmt.Fprintf(os.Stderr, "\ndsh-remote: %s\n", notice)
					if log != nil {
						log.Logf("signal: %s", notice)
					}
				})
				continue
			}
			exitReason = fmt.Sprintf("received %s", sig)
			fmt.Fprintf(os.Stderr, "\ndsh-remote: received %s, shutting down\n", sig)
			// An operator asked for this, so it is a clean exit (status 0) and
			// systemd's Restart=on-failure deliberately leaves it stopped.
			teardown()
			return nil
		case serveErrValue := <-serveErr:
			exitReason = "the stable proxy stopped"
			if serveErrValue != nil {
				exitReason = fmt.Sprintf("the stable proxy stopped: %v", serveErrValue)
				return fmt.Errorf("stable proxy stopped unexpectedly: %w", serveErrValue)
			}
			return errors.New("stable proxy stopped unexpectedly")
		case <-proc.Done():
			reason := dsh.ExitReason(proc.Err())
			exitReason = "DeepSeek Harness " + reason
			return fmt.Errorf("DeepSeek Harness %s; stopping dsh-remote", reason)
		}
	}
}

// signalAction is what the supervise loop does with a signal it received.
type signalAction int

const (
	// signalStop is an explicit stop request from the operator or a supervisor:
	// shut down cleanly and record why.
	signalStop signalAction = iota
	// signalContinue is terminal or session noise, not a stop request: keep
	// serving. A hangup means the terminal went away, which is exactly when a
	// remote URL needs to stay up.
	signalContinue
)

// actionFor classifies a delivered signal. Only explicit stop requests may end
// the run: treating a hangup as one is what killed the proxy silently, left a
// stale state file, and stranded a DeepSeek Harness holding the port.
func actionFor(sig os.Signal) signalAction {
	switch sig {
	case os.Interrupt, syscall.SIGTERM:
		return signalStop
	default:
		return signalContinue
	}
}

// describeExit renders the supervise outcome for the log. The error is the
// fallback, so no exit path can be recorded as unexplained: if a reason was not
// assigned explicitly, the failure that caused the exit still names itself.
func describeExit(reason string, cause error) string {
	if reason == "" {
		if cause != nil {
			return "failed: " + cause.Error()
		}
		return "unknown (teardown ran without recording a reason or an error)"
	}
	return reason
}

// serveMode renders whether Tailscale Serve is managed, for the log header.
func serveMode(noServe bool) string {
	if noServe {
		return "not managed (--no-serve)"
	}
	return "managed"
}

// describeSpec renders how DeepSeek Harness will be launched, without any
// credential: only the executable, the ports, and the trusted authorities.
func describeSpec(spec dsh.Spec) string {
	exec := spec.Exec
	if exec == "" {
		exec = "npx " + spec.Package
	}
	line := fmt.Sprintf("%s web --no-open --port %d", exec, spec.Port)
	for _, host := range spec.TrustedHosts {
		if host != "" {
			line += " --trusted-host " + host
		}
	}
	return line
}

// ensureServePermission applies one Serve root mapping, and when Tailscale
// refuses for lack of privilege, offers the one-time operator grant on the
// controlling terminal. Without a terminal -- notably under the systemd user
// service -- it never attempts sudo; it fails with the exact command instead.
func ensureServePermission(ctx context.Context, ts *tailscale.Client, mount string) error {
	err := ts.SetRootProxy(ctx, mount)
	if err == nil {
		return nil
	}
	var perm *tailscale.PermissionError
	if !errors.As(err, &perm) {
		return err
	}

	granted, grantErr := offerOperatorGrant(ctx)
	if grantErr != nil && !errors.Is(grantErr, errNoTerminal) {
		return fmt.Errorf("%w\n\n%s", grantErr, perm.Guidance())
	}
	if !granted {
		return fmt.Errorf("%w\n\n%s", perm, perm.Guidance())
	}
	fmt.Fprintln(os.Stderr, "Retrying Tailscale Serve...")
	if err := ts.SetRootProxy(ctx, mount); err != nil {
		return err
	}
	return nil
}

// printStartSummary prints the human-facing startup block. The tailnet URL is
// for the phone (and anything off the local network); the local network URL is
// for a browser on the same network, which then needs no Tailscale at all.
func printStartSummary(opts startOptions, tailnetHost, lanAddr string, dshPID int, services []process.ServiceState) {
	fmt.Printf("DSH listening on %s:%d\n", dsh.Loopback, opts.dshPort)
	fmt.Printf("DSH PID: %d\n", dshPID)
	fmt.Printf("Proxy listening on %s:%d\n", dsh.Loopback, opts.proxyPort)
	if lanAddr != "" {
		fmt.Printf("Proxy also listening on %s:%d (local network)\n", lanAddr, opts.proxyPort)
	}
	if tailnetHost != "" {
		if opts.noServe {
			fmt.Printf("Tailscale Serve: not managed (--no-serve)\n")
		}
		fmt.Printf("Remote URL: https://%s%s\n", tailnetHost, proxy.BootstrapPath)
	} else {
		fmt.Printf("Tailscale Serve: not configured\n")
		fmt.Printf("Local URL: http://%s:%d%s\n", dsh.Loopback, opts.proxyPort, proxy.BootstrapPath)
	}
	if lanAddr != "" {
		fmt.Printf("Local network URL: http://%s%s\n", net.JoinHostPort(lanAddr, strconv.Itoa(opts.proxyPort)), proxy.BootstrapPath)
	}
	for _, svc := range services {
		fmt.Printf("%s backend: %s\n", svc.Name, svc.Target)
		if svc.TailnetURL != "" {
			fmt.Printf("%s remote URL: %s\n", svc.Name, svc.TailnetURL)
		}
		if svc.LANURL != "" {
			fmt.Printf("%s local network URL: %s\n", svc.Name, svc.LANURL)
		}
	}
}

// restoreServe puts back the handler dsh-remote replaced, or explains why it
// cannot safely remove the one it created. It never calls `tailscale serve
// reset`, which would wipe unrelated Serve configuration.
func restoreServe(ts *tailscale.Client, priorTarget string, proxyPort int, log *logging.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if priorTarget != "" {
		if err := ts.SetRootProxy(ctx, priorTarget); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not restore the previous tailscale serve mapping (%s); run `tailscale serve view` to check\n", priorTarget)
			if log != nil {
				log.Logf("tailscale: could not restore serve / -> %s: %v", priorTarget, err)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Restored tailscale serve / -> %s\n", priorTarget)
			if log != nil {
				log.Logf("tailscale: restored serve / -> %s", priorTarget)
			}
		}
		return
	}
	fmt.Fprintf(os.Stderr, "note: tailscale serve still maps / to the now-stopped proxy on port %d.\n"+
		"      dsh-remote will not run `tailscale serve reset` because that would wipe unrelated Serve config.\n"+
		"      To remove just this mapping, use `tailscale serve set-config` or reset Serve deliberately.\n", proxyPort)
	if log != nil {
		log.Logf("tailscale: leaving existing serve mapping to the stopped proxy on port %d", proxyPort)
	}
}

// verifyLANListener proves the local-network listener is bound to a real
// interface address and accepts connections there, rather than existing only on
// loopback while the advertised URL claims otherwise.
func verifyLANListener(ln net.Listener, lanAddr string, port int, log *logging.Logger) error {
	bound := ln.Addr().String()
	if host, _, err := net.SplitHostPort(bound); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return fmt.Errorf("--lan-address %s bound the local network listener to loopback (%s); pass a real local network address", lanAddr, bound)
		}
	}

	// A connection to the address we are about to advertise. The socket is
	// already listening, so this succeeds unless a local firewall or routing
	// rule intervenes -- which the operator needs to know about, but which must
	// not stop a proxy that is correctly bound to a non-loopback address.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(lanAddr, strconv.Itoa(port)), 2*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: the proxy is bound on %s but a test connection to %s failed: %v\n"+
			"         clients on the local network may not reach it\n",
			bound, net.JoinHostPort(lanAddr, strconv.Itoa(port)), err)
		if log != nil {
			log.Logf("lan: listener on %s did not accept a test connection: %v", bound, err)
		}
		return nil
	}
	_ = conn.Close()
	if log != nil {
		log.Logf("lan: listener verified on %s", bound)
	}
	return nil
}

// warnIfUnmanagedNextToService flags the lifecycle trap behind the reported
// "the service is inactive but status says a proxy is running" observation: a
// manually started proxy holds the ports while the installed unit sits
// inactive, so nothing would restart it. It also explains why a later
// `systemctl --user start` is refused -- the run-state guard sees the running
// proxy first.
func warnIfUnmanagedNextToService(log *logging.Logger) {
	if runningAsService() {
		return // systemd started this run; the unit owns it
	}
	path, err := systemd.New().UnitPath()
	if err != nil {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "note: %s exists, but this run is not managed by systemd.\n", path)
	fmt.Fprintln(os.Stderr, "      The service stays inactive, so this proxy will not be restarted automatically.")
	fmt.Fprintln(os.Stderr, "      For a supervised run, stop this and use `systemctl --user start "+systemd.UnitName+"`.")
	if log != nil {
		log.Logf("supervision: %s exists but this run is not managed by systemd", path)
	}
}

// warnIfTTYWithoutSupervisor names the launch context that caused the reported
// silent deaths: a foreground child of an interactive shell, with no service
// manager, whose stdout is a terminal (or a pipe to one).
//
// Such a run is one SIGHUP, or one SIGPIPE on a vanished output pipe, away from
// dying without running any teardown -- leaving a stale state file and an
// orphaned DeepSeek Harness holding the port. The supervisor is what makes the
// difference between "stopped" and "vanished", so say so at start time rather
// than let the operator discover it from an outage.
func warnIfTTYWithoutSupervisor(log *logging.Logger) {
	if runningAsService() {
		return // systemd started this run; it has no controlling terminal
	}
	if !hasControllingTTY() {
		return
	}
	fmt.Fprintln(os.Stderr, "note: this run has a controlling terminal and no service manager.")
	fmt.Fprintln(os.Stderr, "      It survives the terminal going away, but nothing restarts it if it crashes.")
	fmt.Fprintln(os.Stderr, "      For a supervised run: `dsh-remote install` then `systemctl --user enable --now "+systemd.UnitName+"`.")
	if log != nil {
		log.Logf("supervision: no service manager and a controlling terminal; nothing would restart this run after a crash")
	}
}

// hasControllingTTY reports whether this process is attached to a terminal. It
// asks /dev/tty, which is the same question the kernel answers when it decides
// where to deliver SIGHUP.
func hasControllingTTY() bool {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	return true
}
