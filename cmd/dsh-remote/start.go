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
	lan          bool
	lanAddress   string
	noServe      bool
	noQR         bool
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

// runStart is declared with a named error result so the deferred teardown can
// record why the run ended. Without it, a preflight refusal or a bind failure
// was logged as "shutdown: unknown", which reads like an unexplained crash --
// exactly what the persistent log exists to rule out.
func runStart(args []string) (err error) {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	var opts startOptions
	fs.IntVar(&opts.dshPort, "dsh-port", defaultDSHPort, "loopback port for DeepSeek Harness")
	fs.IntVar(&opts.proxyPort, "proxy-port", defaultProxyPort, "loopback port for the stable proxy")
	fs.StringVar(&opts.npx, "npx", "npx", "npx executable to launch DeepSeek Harness with")
	fs.StringVar(&opts.dshExec, "dsh-exec", "", "run this dsh executable directly instead of via npx")
	fs.StringVar(&opts.tailnetHost, "tailnet-host", "", "override the detected Tailscale hostname")
	fs.Var(&opts.trustedHosts, "trusted-host", "extra authority to pass to dsh --trusted-host (repeatable)")
	fs.BoolVar(&opts.lan, "lan", false, "also serve the stable URL on this host's local network address (plain HTTP)")
	fs.StringVar(&opts.lanAddress, "lan-address", "", "local network address to bind for --lan (default: detected)")
	fs.BoolVar(&opts.noServe, "no-serve", false, "do not touch the Tailscale Serve configuration")
	fs.BoolVar(&opts.noQR, "no-qr", false, "do not print a QR code")
	fs.DurationVar(&opts.tokenTimeout, "token-timeout", 90*time.Second, "how long to wait for DeepSeek Harness to print its token URL")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: dsh-remote start [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
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
	if existing != nil && process.Alive(existing.PID) {
		if log != nil {
			log.Logf("refusing to start: recorded proxy pid %d is still alive", existing.PID)
			_ = log.Close()
		}
		return fmt.Errorf("dsh-remote is already running (pid %d); run `dsh-remote stop` first", existing.PID)
	}

	var (
		ts              *tailscale.Client
		proc            *dsh.Process
		server          *proxy.Server
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
			if proc != nil {
				_ = proc.Stop(5 * time.Second)
				if log != nil {
					log.Logf("dsh: stopped (pid %d)", proc.PID())
				}
			}
			if serveConfigured {
				restoreServe(ts, priorTarget, opts.proxyPort, log)
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
			lanAddr = addresses[0]
			if len(addresses) > 1 {
				fmt.Fprintf(os.Stderr, "note: local network addresses %v; using %s (override with --lan-address)\n", addresses, lanAddr)
			}
		}
		fmt.Fprintf(os.Stderr, "warning: --lan publishes %s over plain HTTP to the local network; "+
			"anyone on it can open %s and obtain a DeepSeek Harness session\n", lanAddr, proxy.BootstrapPath)
		if err := process.PortAvailable(lanAddr, opts.proxyPort); err != nil {
			return err
		}
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
		listeners = append(listeners, lanListener)
	}
	serveErr := make(chan error, len(listeners))
	for _, listener := range listeners {
		if log != nil {
			log.Logf("proxy: listening on %s", listener.Addr())
		}
		go func() { serveErr <- server.Serve(listener) }()
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
	}
	if tailnetHost != "" {
		state.RemoteURL = "https://" + tailnetHost + proxy.BootstrapPath
	}
	if lanAddr != "" {
		state.LANURL = "http://" + net.JoinHostPort(lanAddr, strconv.Itoa(opts.proxyPort)) + proxy.BootstrapPath
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
	printStartSummary(opts, tailnetHost, lanAddr, proc.PID())
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
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
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
func printStartSummary(opts startOptions, tailnetHost, lanAddr string, dshPID int) {
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
