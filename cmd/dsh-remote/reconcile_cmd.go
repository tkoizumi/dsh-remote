package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

const reconcileUsageText = `dsh-remote reconcile - clear a leftover DeepSeek Harness and restore service

After an unclean exit (SIGKILL, a closed terminal that skips SIGTERM, a crash),
the proxy is gone but the DeepSeek Harness it launched can still hold its port.
Every supervised restart then fails on that port conflict, so the outage outlives
the process that caused it.

reconcile stops that leftover process -- only when its ownership is provable from
the recorded run state -- clears the stale state file, and then starts a fresh
proxy. A DeepSeek Harness that dsh-remote did not launch is never touched.

  --no-start   clean up but do not start a new proxy
  --yes        clean up without an interactive prompt
`

func runReconcile(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	var opts statusOptions
	fs.IntVar(&opts.dshPort, "dsh-port", defaultDSHPort, "loopback port DeepSeek Harness listens on")
	fs.IntVar(&opts.proxyPort, "proxy-port", defaultProxyPort, "loopback port the stable proxy listens on")
	noStart := fs.Bool("no-start", false, "clean up leftover processes but do not start a new proxy")
	yes := fs.Bool("yes", false, "skip the interactive confirmation")
	fs.Usage = func() { fmt.Fprint(os.Stderr, reconcileUsageText) }
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	d := runDiagnose(ctx, opts)
	sit := classify(d)

	fmt.Println()
	switch sit {
	case situationHealthy:
		fmt.Println("Nothing to reconcile: the proxy is healthy and remote access works.")
		return nil
	case situationPortTaken:
		return fmt.Errorf("port %d is held by a process dsh-remote did not launch; reconcile will not stop it", opts.dshPort)
	}

	fmt.Printf("Current state: %s\n", sit)
	fmt.Print(reasonLine(d, sit, opts))

	// Ask before terminating anything. Without a terminal the question cannot be
	// answered, so refuse rather than kill unattended.
	if !*yes {
		confirmed, err := confirmReconcile()
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Println("Nothing was changed.")
			return nil
		}
	}

	preflight, err := dshPortPreflight(opts.dshPort, d.State, func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	})
	if err != nil {
		fmt.Printf("Reconcile could not finish: %v\n", err)
		return err
	}
	fmt.Printf("Preflight: %s\n", describePreflight(preflight))

	if *noStart {
		fmt.Println("Left stopped (--no-start). Start again with `dsh-remote start`.")
		return nil
	}

	// If systemd is actively supervising, let it own the restart: starting a
	// second copy here would collide with the unit.
	if d.Supervision.Installed && d.Supervision.Active {
		fmt.Println("The supervised service is active; it owns the restart.")
		fmt.Println("    systemctl --user restart " + "dsh-remote.service")
		fmt.Println("Wait a few seconds, then run `dsh-remote status`.")
		return nil
	}

	fmt.Println("Starting a fresh proxy...")
	return runStart([]string{
		"--dsh-port", fmt.Sprint(opts.dshPort),
		"--proxy-port", fmt.Sprint(opts.proxyPort),
	})
}

// confirmReconcile asks on the controlling terminal. Without one it refuses,
// because terminating a running service unattended is never the safe default.
func confirmReconcile() (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, fmt.Errorf("refusing to stop a running DeepSeek Harness without confirmation; re-run with --yes to proceed: %w", err)
	}
	defer tty.Close()
	fmt.Fprint(tty, "Stop the leftover DeepSeek Harness and start a fresh proxy? [y/N] ")
	var answer string
	if _, err := fmt.Fscanln(tty, &answer); err != nil {
		return false, nil
	}
	switch answer {
	case "y", "Y", "yes", "YES", "Yes":
		return true, nil
	}
	return false, nil
}
