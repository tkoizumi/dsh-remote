package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/dsh"
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

func runStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	var opts startOptions
	fs.IntVar(&opts.dshPort, "dsh-port", defaultDSHPort, "loopback port for DeepSeek Harness")
	fs.IntVar(&opts.proxyPort, "proxy-port", defaultProxyPort, "loopback port for the stable proxy")
	fs.StringVar(&opts.npx, "npx", "npx", "npx executable to launch DeepSeek Harness with")
	fs.StringVar(&opts.dshExec, "dsh-exec", "", "run this dsh executable directly instead of via npx")
	fs.StringVar(&opts.tailnetHost, "tailnet-host", "", "override the detected Tailscale hostname")
	fs.Var(&opts.trustedHosts, "trusted-host", "extra authority to pass to dsh --trusted-host (repeatable)")
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

	if existing, err := process.LoadState(); err != nil {
		return err
	} else if existing != nil && process.Alive(existing.PID) {
		return fmt.Errorf("dsh-remote is already running (pid %d); run `dsh-remote stop` first", existing.PID)
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

	var ts *tailscale.Client
	if !opts.noServe {
		var err error
		ts, err = tailscale.New()
		if err != nil {
			return err
		}
	}

	if err := process.PortAvailable(dsh.Loopback, opts.dshPort); err != nil {
		return err
	}
	if err := process.PortAvailable(dsh.Loopback, opts.proxyPort); err != nil {
		return err
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
	// authorities, and the phone reaches it under the tailnet name.
	if tailnetHost != "" {
		spec.TrustedHosts = append(spec.TrustedHosts, tailnetHost)
	}
	spec.TrustedHosts = append(spec.TrustedHosts, opts.trustedHosts...)

	// --- Teardown ----------------------------------------------------------
	// Defined before anything is started so every error path unwinds cleanly.
	var (
		proc            *dsh.Process
		server          *proxy.Server
		priorTarget     string
		serveConfigured bool
		stateSaved      bool
	)
	var teardownOnce sync.Once
	teardown := func() {
		teardownOnce.Do(func() {
			if server != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = server.Shutdown(shutdownCtx)
				cancel()
			}
			if proc != nil {
				_ = proc.Stop(5 * time.Second)
			}
			if serveConfigured {
				restoreServe(ts, priorTarget, opts.proxyPort)
			}
			if stateSaved {
				if err := process.RemoveState(); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not remove state file: %v\n", err)
				}
			}
		})
	}
	defer teardown()

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
	fmt.Fprintln(os.Stderr, "Starting DeepSeek Harness...")
	var err error
	proc, err = dsh.Start(ctx, spec, os.Stderr)
	if err != nil {
		return err
	}

	if _, err := proc.WaitToken(opts.tokenTimeout); err != nil {
		return err
	}

	// --- Stable proxy ------------------------------------------------------
	upstream, err := url.Parse("http://" + dsh.Loopback + ":" + strconv.Itoa(opts.dshPort))
	if err != nil {
		return err
	}
	server = proxy.New(upstream, func() string { return proc.Token().Token }, version)
	listener, err := proxy.Listen(opts.proxyPort)
	if err != nil {
		return err
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	// Point Serve at the proxy only now that it is listening, so replacing an
	// existing mapping does not open a window of 502s for the current URL.
	if !opts.noServe && !serveConfigured {
		if err := ts.SetRootProxy(ctx, target); err != nil {
			return fmt.Errorf("configure tailscale serve: %w", err)
		}
		serveConfigured = true
	}

	// --- Persist run state (never the token) -------------------------------
	state := &process.State{
		PID:              os.Getpid(),
		DSHPID:           proc.PID(),
		DSHAddr:          "http://" + dsh.Loopback + ":" + strconv.Itoa(opts.dshPort),
		ProxyAddr:        "http://" + dsh.Loopback + ":" + strconv.Itoa(opts.proxyPort),
		TailnetHost:      tailnetHost,
		PriorServeTarget: priorTarget,
		ServeConfigured:  serveConfigured,
		Version:          version,
		StartedAt:        time.Now(),
	}
	if tailnetHost != "" {
		state.RemoteURL = "https://" + tailnetHost + proxy.BootstrapPath
	}
	if err := process.SaveState(state); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	} else {
		stateSaved = true
	}

	// --- Announce ----------------------------------------------------------
	printStartSummary(opts, tailnetHost, proc.PID())
	if !opts.noQR && state.RemoteURL != "" {
		fmt.Println("Scan this QR code from your phone:")
		if err := qr.Render(os.Stdout, state.RemoteURL); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
	}
	fmt.Fprintln(os.Stderr, "\nPress Ctrl+C to stop. DeepSeek Harness output follows.")

	// --- Supervise ---------------------------------------------------------
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\ndsh-remote: received %s, shutting down\n", sig)
		teardown()
		return nil
	case err := <-serveErr:
		teardown()
		if err != nil {
			return fmt.Errorf("stable proxy stopped unexpectedly: %w", err)
		}
		return errors.New("stable proxy stopped unexpectedly")
	case <-proc.Done():
		reason := dsh.ExitReason(proc.Err())
		teardown()
		return fmt.Errorf("DeepSeek Harness %s; stopping dsh-remote", reason)
	}
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

// printStartSummary prints the human-facing startup block.
func printStartSummary(opts startOptions, tailnetHost string, dshPID int) {
	fmt.Printf("DSH listening on %s:%d\n", dsh.Loopback, opts.dshPort)
	fmt.Printf("DSH PID: %d\n", dshPID)
	fmt.Printf("Proxy listening on %s:%d\n", dsh.Loopback, opts.proxyPort)
	if tailnetHost != "" {
		if opts.noServe {
			fmt.Printf("Tailscale Serve: not managed (--no-serve)\n")
		}
		fmt.Printf("Remote URL: https://%s%s\n", tailnetHost, proxy.BootstrapPath)
		return
	}
	fmt.Printf("Tailscale Serve: not configured\n")
	fmt.Printf("Local URL: http://%s:%d%s\n", dsh.Loopback, opts.proxyPort, proxy.BootstrapPath)
}

// restoreServe puts back the handler dsh-remote replaced, or explains why it
// cannot safely remove the one it created. It never calls `tailscale serve
// reset`, which would wipe unrelated Serve configuration.
func restoreServe(ts *tailscale.Client, priorTarget string, proxyPort int) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if priorTarget != "" {
		if err := ts.SetRootProxy(ctx, priorTarget); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not restore the previous tailscale serve mapping (%s); run `tailscale serve view` to check\n", priorTarget)
		} else {
			fmt.Fprintf(os.Stderr, "Restored tailscale serve / -> %s\n", priorTarget)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "note: tailscale serve still maps / to the now-stopped proxy on port %d.\n"+
		"      dsh-remote will not run `tailscale serve reset` because that would wipe unrelated Serve config.\n"+
		"      To remove just this mapping, use `tailscale serve set-config` or reset Serve deliberately.\n", proxyPort)
}
