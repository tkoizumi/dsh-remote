package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const serviceName = "dsh-remote.service"

func runInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	system := fs.Bool("system", false, "install a system-wide unit instead of a user unit")
	force := fs.Bool("force", false, "overwrite an existing unit file")
	startNow := fs.Bool("start", true, "enable and start the service immediately")
	lan := fs.Bool("lan", false, "have the service serve the stable URL on the local network too")
	lanAddress := fs.String("lan-address", "", "local network address for --lan (default: detected)")
	noServe := fs.Bool("no-serve", false, "have the service run without Tailscale Serve (use this in a VM)")
	dshExec := fs.String("dsh-exec", "", "run this dsh executable instead of via npx")
	var trustedHosts stringList
	fs.Var(&trustedHosts, "trusted-host", "extra authority to pass to dsh --trusted-host (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve the dsh-remote executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}

	startFlags := installStartFlags(*lan, *lanAddress, *noServe, *dshExec, trustedHosts)
	unit := renderUnit(executable, startFlags, *noServe)

	var unitPath string
	if *system {
		unitPath = filepath.Join("/etc/systemd/system", serviceName)
	} else {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("resolve user config directory: %w", err)
		}
		unitPath = filepath.Join(configDir, "systemd", "user", serviceName)
	}

	if _, err := os.Stat(unitPath); err == nil && !*force {
		return fmt.Errorf("%s already exists; re-run with --force to overwrite", unitPath)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(unitPath), err)
	}
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", unitPath, err)
	}
	fmt.Printf("Wrote %s\n", unitPath)

	systemctl := []string{"systemctl"}
	if !*system {
		systemctl = append(systemctl, "--user")
	}
	reload := append(append([]string{}, systemctl...), "daemon-reload")
	if err := runCommand(reload); err != nil {
		printManualSteps(systemctl, unitPath, *startNow)
		return nil
	}
	if *startNow {
		enable := append(append([]string{}, systemctl...), "enable", "--now", serviceName)
		if err := runCommand(enable); err != nil {
			printManualSteps(systemctl, unitPath, *startNow)
			return nil
		}
		fmt.Printf("Enabled and started %s\n", serviceName)
	}

	fmt.Println()
	fmt.Println("Notes:")
	if !*noServe {
		fmt.Println("  - Tailscale Serve needs root or an operator. If dsh-remote reports")
		fmt.Println("    'serve config denied', enable it once with:")
		fmt.Println("        sudo tailscale set --operator=$USER")
	}
	if !*system {
		fmt.Println("  - For the user service to start before you log in, run:")
		fmt.Println("        sudo loginctl enable-linger $USER")
	}
	return nil
}

func runUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	system := fs.Bool("system", false, "remove the system-wide unit instead of the user unit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	systemctl := []string{"systemctl"}
	if !*system {
		systemctl = append(systemctl, "--user")
	}
	_ = runCommand(append(append([]string{}, systemctl...), "disable", "--now", serviceName))

	var unitPath string
	if *system {
		unitPath = filepath.Join("/etc/systemd/system", serviceName)
	} else {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		unitPath = filepath.Join(configDir, "systemd", "user", serviceName)
	}
	if err := os.Remove(unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", unitPath, err)
	}
	fmt.Printf("Removed %s\n", unitPath)
	_ = runCommand(append(append([]string{}, systemctl...), "daemon-reload"))
	return nil
}

// installStartFlags renders the `start` flags the installed service should use.
// It is separate from runInstall so the flag wiring is testable without
// touching systemd or the filesystem.
func installStartFlags(lan bool, lanAddress string, noServe bool, dshExec string, trustedHosts []string) string {
	flags := ""
	if lan {
		flags += " --lan"
		if lanAddress != "" {
			flags += " --lan-address=" + systemdArg(lanAddress)
		}
	}
	if noServe {
		flags += " --no-serve"
	}
	if dshExec != "" {
		flags += " --dsh-exec=" + systemdArg(dshExec)
	}
	for _, host := range trustedHosts {
		flags += " --trusted-host=" + systemdArg(host)
	}
	return flags
}

func renderUnit(executable, startFlags string, noServe bool) string {
	// A VM without Tailscale has no tailscaled.service, so do not order after it.
	after := "network-online.target"
	if !noServe {
		after += " tailscaled.service"
	}
	// Record --no-serve in the environment as well as the command line, so a
	// later `dsh-remote status` or `doctor` inside this unit can tell that Serve
	// is deliberately unmanaged rather than broken.
	noServeEnvLine := ""
	if noServe {
		noServeEnvLine = "\nEnvironment=" + noServeEnv + "=1"
	}
	return fmt.Sprintf(`[Unit]
Description=dsh-remote: a stable /dsh URL for DeepSeek Harness
Documentation=https://github.com/tkoizumi/dsh-remote
After=%s
Wants=network-online.target

[Service]
Type=simple
# Capture the install-time PATH so npx (often installed outside /usr/bin) is
# still resolvable under systemd's minimal environment.
Environment=PATH=%s%s
ExecStart=%s start%s
Restart=on-failure
RestartSec=3
TimeoutStopSec=25

[Install]
WantedBy=default.target
`, after, os.Getenv("PATH"), noServeEnvLine, systemdEscape(executable), startFlags)
}

// systemdArg quotes one ExecStart argument value when it contains characters
// systemd would otherwise split or treat specially. The flag name stays
// outside the quotes so `--flag=value` remains a single word.
func systemdArg(value string) string {
	if !strings.ContainsAny(value, " \t\"'\\%") {
		return value
	}
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`)
	return `"` + replacer.Replace(value) + `"`
}

// systemdEscape quotes a path for an ExecStart= line.
func systemdEscape(path string) string {
	if !strings.ContainsAny(path, " \t\"'\\") {
		return path
	}
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`)
	return `"` + replacer.Replace(path) + `"`
}

func runCommand(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func printManualSteps(systemctl []string, unitPath string, startNow bool) {
	fmt.Fprintln(os.Stderr, "Could not talk to systemd automatically. Finish with:")
	fmt.Fprintf(os.Stderr, "    %s daemon-reload\n", strings.Join(systemctl, " "))
	if startNow {
		fmt.Fprintf(os.Stderr, "    %s enable --now %s\n", strings.Join(systemctl, " "), serviceName)
	}
}
