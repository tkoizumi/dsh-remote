package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/lima"
	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/proxy"
)

// defaultVMName is the Lima instance dsh-remote manages unless told otherwise.
const defaultVMName = "dsh"

// guestDSHPathFile is written by the guest provisioning script; see the
// provision constant in internal/lima. Reading it with `cat` keeps the host
// from needing a shell (and therefore quoting) over `limactl shell`.
const guestDSHPathFile = "/usr/local/share/dsh-remote/dsh-exec"

const vmUsageText = `dsh-remote vm - run DeepSeek Harness in a Lima virtual machine

Usage:
  dsh-remote vm start [flags]   Create or start the VM, install the service, report the URL
  dsh-remote vm stop [flags]    Stop DeepSeek Harness inside the VM
  dsh-remote vm shell [flags]   Open a shell inside the VM
  dsh-remote vm status [flags]  Report the VM and the guest service

Run "dsh-remote vm <subcommand> -h" for command-specific flags.
`

func runVM(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, vmUsageText)
		return errors.New("vm needs a subcommand")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "start":
		return runVMStart(rest)
	case "stop":
		return runVMStop(rest)
	case "shell":
		return runVMShell(rest)
	case "status":
		return runVMStatus(rest)
	case "help", "-h", "--help":
		fmt.Print(vmUsageText)
		return nil
	default:
		fmt.Fprint(os.Stderr, vmUsageText)
		return fmt.Errorf("unknown vm subcommand %q", sub)
	}
}

func runVMStart(args []string) error {
	fs := flag.NewFlagSet("vm start", flag.ContinueOnError)
	name := fs.String("name", defaultVMName, "Lima instance name")
	cpus := fs.Int("cpus", lima.Defaults.CPUs, "virtual CPUs for the VM")
	memory := fs.String("memory", lima.Defaults.Memory, "memory for the VM")
	disk := fs.String("disk", lima.Defaults.Disk, "disk for the VM")
	mount := fs.String("mount", lima.Defaults.Mount, "host directory to share writable into the VM")
	configPath := fs.String("config", "", "use this Lima config file instead of a generated one")
	proxyPort := fs.Int("proxy-port", defaultProxyPort, "loopback port the guest proxy listens on")
	var trustedHosts stringList
	fs.Var(&trustedHosts, "trusted-host", "extra authority for the guest dsh --trusted-host (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	ctx := context.Background()
	lc, err := ensureLima(ctx)
	if err != nil {
		return err
	}

	inst, err := lc.Find(ctx, *name)
	if err != nil {
		return err
	}
	switch {
	case inst == nil:
		path := *configPath
		if path == "" {
			cfg := lima.Config{CPUs: *cpus, Memory: *memory, Disk: *disk, Mount: *mount}
			path, err = writeVMConfig(*name, cfg)
			if err != nil {
				return err
			}
		}
		fmt.Fprintf(os.Stderr, "Creating the Lima instance %q; this downloads an image and can take a few minutes...\n", *name)
		if err := lc.Start(ctx, *name, path); err != nil {
			return err
		}
	case !inst.Running():
		fmt.Fprintf(os.Stderr, "Starting the Lima instance %q...\n", *name)
		if err := lc.Start(ctx, *name, ""); err != nil {
			return err
		}
	default:
		fmt.Fprintf(os.Stderr, "Lima instance %q is already running.\n", *name)
	}

	if err := ensureGuestBinary(ctx, lc, *name); err != nil {
		return err
	}
	if err := installGuestService(ctx, lc, *name, trustedHosts); err != nil {
		return err
	}

	addr := "http://127.0.0.1:" + strconv.Itoa(*proxyPort)
	fmt.Fprintln(os.Stderr, "Waiting for DeepSeek Harness to come up in the VM...")
	if err := waitForProxy(ctx, addr, 2*time.Minute); err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("DeepSeek Harness is running in the Lima instance %q.\n", *name)
	fmt.Printf("Open:  %s%s\n", addr, proxy.BootstrapPath)
	fmt.Printf("Shell: dsh-remote vm shell --name %s\n", *name)
	fmt.Printf("Stop:  dsh-remote vm stop --name %s\n", *name)
	return nil
}

func runVMStop(args []string) error {
	fs := flag.NewFlagSet("vm stop", flag.ContinueOnError)
	name := fs.String("name", defaultVMName, "Lima instance name")
	stopVM := fs.Bool("vm", false, "also shut down the virtual machine")
	if err := fs.Parse(args); err != nil {
		return err
	}

	lc, err := lima.New()
	if err != nil {
		return err
	}
	ctx := context.Background()

	inst, err := lc.Find(ctx, *name)
	if err != nil {
		return err
	}
	if inst == nil {
		fmt.Printf("No Lima instance named %q.\n", *name)
		return nil
	}

	if inst.Running() {
		if _, err := lc.Guest(ctx, *name, "systemctl", "--user", "stop", "dsh-remote"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not stop the guest service: %v\n", err)
		} else {
			fmt.Println("Stopped DeepSeek Harness in the VM.")
		}
	}

	if *stopVM {
		if err := lc.Stop(ctx, *name); err != nil {
			return err
		}
		fmt.Printf("Stopped the Lima instance %q.\n", *name)
	}
	return nil
}

func runVMShell(args []string) error {
	fs := flag.NewFlagSet("vm shell", flag.ContinueOnError)
	name := fs.String("name", defaultVMName, "Lima instance name")
	if err := fs.Parse(args); err != nil {
		return err
	}

	lc, err := lima.New()
	if err != nil {
		return err
	}
	cmd := lc.Interactive(*name, fs.Args()...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runVMStatus(args []string) error {
	fs := flag.NewFlagSet("vm status", flag.ContinueOnError)
	name := fs.String("name", defaultVMName, "Lima instance name")
	proxyPort := fs.Int("proxy-port", defaultProxyPort, "loopback port the guest proxy listens on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	lc, err := lima.New()
	if err != nil {
		return err
	}
	ctx := context.Background()

	inst, err := lc.Find(ctx, *name)
	if err != nil {
		return err
	}

	fmt.Println()
	if inst == nil {
		fmt.Printf("VM %q: not created (run `dsh-remote vm start`)\n", *name)
		return nil
	}
	fmt.Printf("VM %q: %s\n", *name, inst.Status)
	if !inst.Running() {
		return nil
	}

	out, err := lc.Guest(ctx, *name, "systemctl", "--user", "is-active", "dsh-remote")
	status := strings.TrimSpace(out)
	if status == "" && err != nil {
		status = "unknown"
	}
	fmt.Printf("Guest dsh-remote: %s\n", status)

	addr := "http://127.0.0.1:" + strconv.Itoa(*proxyPort)
	if health, ok := fetchHealth(addr); ok {
		fmt.Printf("Host URL: %s%s (token captured: %t)\n", addr, proxy.BootstrapPath, health.TokenCaptured)
	} else {
		fmt.Printf("Host URL: %s%s (not answering)\n", addr, proxy.BootstrapPath)
	}
	return nil
}

// ensureLima resolves limactl, offering to install Lima with Homebrew when it
// is missing. Like offerOperatorGrant, the question is asked on the controlling
// terminal and defaults to no; without a terminal it reports the command and
// fails rather than installing unattended.
func ensureLima(ctx context.Context) (*lima.Client, error) {
	client, err := lima.New()
	if err == nil {
		return client, nil
	}
	if !errors.Is(err, lima.ErrNotInstalled) {
		return nil, err
	}

	brew, lookErr := exec.LookPath("brew")
	if lookErr != nil {
		return nil, fmt.Errorf("%w; Homebrew is not installed either (https://brew.sh)", err)
	}

	granted, grantErr := offerLimaInstall(ctx, brew)
	if grantErr != nil && !errors.Is(grantErr, errNoTerminal) {
		return nil, grantErr
	}
	if !granted {
		return nil, fmt.Errorf("%w; run `%s install lima` when you are ready", err, brew)
	}

	// Re-resolve rather than trusting this process's PATH to have been updated.
	if path, err := lima.Lookup(); err == nil {
		return &lima.Client{Bin: path}, nil
	}
	return nil, errors.New("Lima was installed but limactl is still not on PATH; open a new terminal and try again")
}

// offerLimaInstall asks whether to run `brew install lima`, and runs it if the
// answer is yes. The question is asked on the controlling terminal (/dev/tty),
// never on stdin, so a redirected stdin cannot turn this into an unattended
// install.
func offerLimaInstall(ctx context.Context, brew string) (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, errNoTerminal
	}
	defer tty.Close()

	fmt.Fprint(tty, "\nLima is not installed; dsh-remote vm runs DeepSeek Harness in a Lima VM.\n"+
		"Run `brew install lima` now? [y/N] ")
	answer, _ := bufio.NewReader(tty).ReadString('\n')
	if !confirmYes(answer) {
		return false, nil
	}

	cmd := exec.CommandContext(ctx, brew, "install", "lima")
	cmd.Stdin = tty
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("`brew install lima` failed: %w", err)
	}
	return true, nil
}

// writeVMConfig stores the generated Lima config beside the run state so a
// recreated instance keeps the same shape.
func writeVMConfig(name string, cfg lima.Config) (string, error) {
	statePath, err := process.StatePath()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(statePath), "lima")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, []byte(cfg.YAML()), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// ensureGuestBinary copies this dsh-remote into the VM unless the guest already
// runs the same version, so a locally built binary is what the VM runs.
func ensureGuestBinary(ctx context.Context, lc *lima.Client, name string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve the dsh-remote executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	if out, err := lc.Guest(ctx, name, "dsh-remote", "version"); err == nil {
		if strings.Contains(out, strings.TrimSpace(version)) {
			return nil
		}
	}

	fmt.Fprintln(os.Stderr, "Installing dsh-remote inside the VM...")
	if err := lc.Copy(ctx, []string{exe}, name+":/tmp/dsh-remote"); err != nil {
		return err
	}
	if _, err := lc.Guest(ctx, name, "sudo", "install", "-m755", "/tmp/dsh-remote", "/usr/local/bin/dsh-remote"); err != nil {
		return err
	}
	return nil
}

// installGuestService makes the VM run dsh-remote as a Tailscale-free systemd
// user service that survives logout and reboot.
func installGuestService(ctx context.Context, lc *lima.Client, name string, trustedHosts []string) error {
	user, err := lc.Guest(ctx, name, "id", "-un")
	if err != nil {
		return err
	}
	user = strings.TrimSpace(user)
	if user == "" {
		return errors.New("could not determine the VM's username")
	}
	// Linger keeps the user manager alive without an interactive login, so the
	// service comes back after a reboot. It is harmless when already enabled.
	if _, err := lc.Guest(ctx, name, "sudo", "loginctl", "enable-linger", user); err != nil {
		return err
	}

	dshPath := ""
	if out, err := lc.Guest(ctx, name, "cat", guestDSHPathFile); err == nil {
		dshPath = strings.TrimSpace(out)
	}

	args := []string{"dsh-remote", "install", "--no-serve", "--force"}
	if dshPath != "" {
		args = append(args, "--dsh-exec", dshPath)
	}
	for _, host := range trustedHosts {
		args = append(args, "--trusted-host", host)
	}
	if _, err := lc.Guest(ctx, name, args...); err != nil {
		return err
	}
	if _, err := lc.Guest(ctx, name, "systemctl", "--user", "restart", "dsh-remote"); err != nil {
		return err
	}
	return nil
}

// waitForProxy polls the host-side loopback address until the guest proxy has
// captured a DeepSeek Harness token, which proves the whole path end to end.
func waitForProxy(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if health, ok := fetchHealth(addr); ok && health.TokenCaptured {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the VM's dsh-remote did not become ready at %s within %s; inspect it with `dsh-remote vm shell` and `systemctl --user status dsh-remote`", addr, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
