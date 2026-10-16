package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/logging"
	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/proxy"
	"github.com/tkoizumi/dsh-remote/internal/qr"
	"github.com/tkoizumi/dsh-remote/internal/systemd"
	"github.com/tkoizumi/dsh-remote/internal/tailscale"
)

func runStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	proxyPort := fs.Int("proxy-port", defaultProxyPort, "loopback port the stable proxy listens on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// `stop` appends to the same persistent log as `start`, so a stop is
	// recorded next to the run it ended.
	log := openLog(os.Stderr)
	if log != nil {
		defer log.Close()
		log.Logf("stop requested")
	}

	// When systemd is running this deployment, stop it through systemd rather
	// than signalling its pid out from under the unit.
	if handled, err := delegateStop(*proxyPort); handled {
		if err != nil {
			return err
		}
		if log != nil {
			log.Logf("stop: handed to %s", systemd.UnitName)
		}
		sweepLeftoverState(*proxyPort, log)
		fmt.Println("Stopped. Tailscale itself was left untouched.")
		if log != nil {
			log.Logf("stop complete")
		}
		return nil
	}

	start, err := process.LoadState()
	if err != nil {
		return err
	}
	if start == nil {
		fmt.Println("nothing to stop: no dsh-remote run state was found")
		if log != nil {
			log.Logf("state: nothing to stop")
		}
		return nil
	}

	if start.ProxyAlive() {
		fmt.Printf("Stopping dsh-remote (pid %d)...\n", start.PID)
		if log != nil {
			log.Logf("stopping proxy pid %d", start.PID)
		}
		// SIGTERM lets the start process forward the signal to DeepSeek
		// Harness, shut the proxy down, and restore Tailscale Serve.
		_ = process.Terminate(start.PID, 20*time.Second)
	} else {
		fmt.Printf("dsh-remote (pid %d) is not running; cleaning up leftover state\n", start.PID)
		if log != nil {
			log.Logf("state: recorded proxy pid %d was already gone", start.PID)
		}
	}
	if start.DSHAlive() {
		fmt.Printf("Stopping leftover DeepSeek Harness (pid %d)...\n", start.DSHPID)
		if log != nil {
			log.Logf("stopping leftover dsh pid %d", start.DSHPID)
		}
		_ = process.Terminate(start.DSHPID, 10*time.Second)
	}

	// If the start process was killed hard it could not restore Serve or clear
	// state, so finish the job here. Otherwise the file is already gone.
	sweepLeftoverState(*proxyPort, log)
	fmt.Println("Stopped. Tailscale itself was left untouched.")
	if log != nil {
		log.Logf("stop complete")
	}
	return nil
}

// sweepLeftoverState restores Serve and clears run state that an unclean exit
// left behind. It is idempotent: after a clean shutdown the state file is
// already gone and this does nothing.
func sweepLeftoverState(proxyPort int, log *logging.Logger) {
	remaining, err := process.LoadState()
	if err != nil || remaining == nil {
		return
	}
	if remaining.ServeConfigured {
		restoreServeOnStop(remaining.PriorServeTarget, proxyPort, log)
	}
	restoreServiceServeOnStop(remaining.Services, log)
	if err := process.RemoveState(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove state file: %v\n", err)
	} else if log != nil {
		log.Logf("state: removed")
	}
}

// restoreServeOnStop undoes the Serve mapping dsh-remote created, without ever
// wiping unrelated configuration.
func restoreServeOnStop(priorTarget string, proxyPort int, log *logging.Logger) {
	ts, err := tailscale.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot restore tailscale serve: %v\n", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if priorTarget != "" {
		if err := ts.SetRootProxy(ctx, priorTarget); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not restore tailscale serve / -> %s: %v\n", priorTarget, err)
		} else {
			fmt.Fprintf(os.Stderr, "Restored tailscale serve / -> %s\n", priorTarget)
			if log != nil {
				log.Logf("tailscale: restored serve / -> %s", priorTarget)
			}
		}
		return
	}
	fmt.Fprintf(os.Stderr, "note: tailscale serve still maps / to the stopped proxy on port %d; "+
		"remove it deliberately (dsh-remote never wipes unrelated Serve config)\n", proxyPort)
}

// restoreServiceServeOnStop clears or restores the Serve slots of extra
// dashboards after an unclean proxy exit, when the proxy's own teardown could
// not run.
func restoreServiceServeOnStop(services []process.ServiceState, log *logging.Logger) {
	var ts *tailscale.Client
	for _, svc := range services {
		if !svc.ServeConfigured || svc.HTTPSPort <= 0 {
			continue
		}
		if ts == nil {
			client, err := tailscale.New()
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: cannot clean up tailscale serve for service %s: %v\n", svc.Name, err)
				return
			}
			ts = client
		}
		path := svc.Path
		if path == "" {
			path = servicePath
		}
		restoreServeSlot(ts, svc.HTTPSPort, path, svc.PriorServeTarget, log)
	}
}

func runQR(args []string) error {
	fs := flag.NewFlagSet("qr", flag.ContinueOnError)
	lan := fs.Bool("lan", false, "encode the local network URL instead of the Tailscale URL")
	if err := fs.Parse(args); err != nil {
		return err
	}

	state, err := process.LoadState()
	if err != nil {
		return err
	}
	if *lan {
		if state == nil || state.LANURL == "" {
			return errors.New("no local network URL recorded; start with --lan first")
		}
		fmt.Println(state.LANURL)
		return qr.Render(os.Stdout, state.LANURL)
	}

	remoteURL := ""
	if state != nil {
		remoteURL = state.RemoteURL
	}
	if remoteURL == "" {
		ts, err := tailscale.New()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		host, err := ts.DNSName(ctx)
		if err != nil {
			return err
		}
		remoteURL = "https://" + host + proxy.BootstrapPath
	}
	fmt.Println(remoteURL)
	return qr.Render(os.Stdout, remoteURL)
}
