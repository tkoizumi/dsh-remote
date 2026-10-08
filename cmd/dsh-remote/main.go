// Command dsh-remote keeps one stable Tailscale URL pointed at a local
// DeepSeek Harness (`dsh web`) instance, re-capturing the token DeepSeek
// Harness generates on every restart.
//
// DeepSeek Harness itself is never modified: dsh-remote launches the documented
// command, reads the bootstrap URL it prints, and redirects browsers to it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
)

// version is overridable at build time with
// -ldflags "-X main.version=..."
var version = "0.1.0"

const usageText = `dsh-remote - one stable Tailscale URL for a local DeepSeek Harness

Usage:
  dsh-remote start     Launch DeepSeek Harness, the stable proxy, and Tailscale Serve
  dsh-remote status    Report what is currently running, why, and how to fix it
  dsh-remote doctor    Run end-to-end checks and print a verdict
  dsh-remote reconcile Clear a leftover DeepSeek Harness and restore service
  dsh-remote stop      Stop the proxy and DeepSeek Harness, then restore Serve config
  dsh-remote qr        Print a QR code for the stable /dsh URL
  dsh-remote vm        Run DeepSeek Harness in a Lima VM and serve it here
  dsh-remote install   Install a systemd user service that starts dsh-remote at login
  dsh-remote uninstall Remove that service
  dsh-remote version   Print the version

Run "dsh-remote <command> -h" for command-specific flags.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "dsh-remote: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return errors.New("no command given")
	}
	command, rest := args[0], args[1:]
	switch command {
	case "start":
		return runStart(rest)
	case "status":
		return runStatus(rest)
	case "doctor":
		return runDoctor(rest)
	case "reconcile":
		return runReconcile(rest)
	case "stop":
		return runStop(rest)
	case "qr":
		return runQR(rest)
	case "vm":
		return runVM(rest)
	case "install":
		return runInstall(rest)
	case "uninstall":
		return runUninstall(rest)
	case "version", "-V", "--version":
		fmt.Println("dsh-remote", version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return nil
	default:
		fmt.Fprint(os.Stderr, usageText)
		return fmt.Errorf("unknown command %q", command)
	}
}
