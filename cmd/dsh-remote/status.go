package main

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/dsh"
	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/proxy"
	"github.com/tkoizumi/dsh-remote/internal/systemd"
)

type statusOptions struct {
	dshPort   int
	proxyPort int
	// noServe records that the caller runs without Tailscale Serve, in which
	// case a Serve mapping pointing elsewhere is not this program's business.
	noServe bool
}

// situation is the single classified outcome of a diagnostics pass. Every
// user-facing line is derived from it, so `status` and `doctor` cannot disagree.
type situation int

const (
	// situationHealthy: the proxy answers and remote access works.
	situationHealthy situation = iota
	// situationStopped: nothing is running; a supervised start fixes it.
	situationStopped
	// situationProxyDown: DSH is fine but the proxy is gone. This is the
	// incident. A supervised service either recovers it or cannot.
	situationProxyDown
	// situationOrphaned: DSH is still holding its port while the proxy that
	// launched it is gone, so a restart would collide with it.
	situationOrphaned
	// situationPortTaken: something dsh-remote did not launch holds the DSH
	// port. dsh-remote must never touch it.
	situationPortTaken
	// situationProxyUnhealthy: the proxy port is bound but not answering.
	situationProxyUnhealthy
)

func (s situation) String() string {
	switch s {
	case situationHealthy:
		return "healthy"
	case situationStopped:
		return "stopped"
	case situationProxyDown:
		return "proxy-down"
	case situationOrphaned:
		return "orphaned-dsh"
	case situationPortTaken:
		return "port-taken"
	case situationProxyUnhealthy:
		return "proxy-unhealthy"
	}
	return "unknown"
}

// classify reduces a diagnostics pass to one situation.
func classify(d diagnose) situation {
	if d.Proxy.Healthy {
		return situationHealthy
	}
	// A bound port that is not answering healthily is a different fault from an
	// absent one: the process exists but is broken, and restarting is correct.
	if d.Proxy.Listening && (d.Proxy.Forbidden || d.Proxy.Err != nil) {
		return situationProxyUnhealthy
	}

	// The proxy is absent. What remains on the DSH port decides what a restart
	// would do.
	switch {
	case d.DSH.Listening && d.DSH.Ours:
		// dsh-remote launched this child and its proxy is gone: restarting
		// would collide with the port this child holds.
		return situationOrphaned
	case d.DSH.Listening:
		// Something holds the port, identified or not. dsh-remote must never
		// stop a process it did not launch, so both cases refuse.
		return situationPortTaken
	default:
		return situationStopped
	}
}

// proxyAddress returns the proxy address to report.
func proxyAddress(d diagnose) string { return d.Proxy.Addr }

// dshAddress returns the DSH address to report.
func dshAddress(d diagnose) string { return d.DSH.Addr }

// reachable reports whether the browser-facing URL can work right now.
func reachable(s situation) bool { return s == situationHealthy }

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var opts statusOptions
	fs.IntVar(&opts.dshPort, "dsh-port", defaultDSHPort, "loopback port DeepSeek Harness listens on")
	fs.IntVar(&opts.proxyPort, "proxy-port", defaultProxyPort, "loopback port the stable proxy listens on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	d := runDiagnose(context.Background(), opts)
	fmt.Print(renderStatus(d, opts))
	return nil
}

// renderStatus renders the full status report.
func renderStatus(d diagnose, opts statusOptions) string {
	sit := classify(d)
	var b strings.Builder

	fmt.Fprintf(&b, "\nDSH: %s\n", dshStatusText(d))
	for _, line := range dshPIDLines(d) {
		fmt.Fprintf(&b, "%s\n", line)
	}
	fmt.Fprintf(&b, "DSH local address: %s\n", dshAddress(d))

	fmt.Fprintf(&b, "\nProxy: %s\n", proxyStatusText(d))
	for _, line := range proxyNoteLines(d) {
		fmt.Fprintf(&b, "%s\n", line)
	}
	fmt.Fprintf(&b, "Proxy local address: %s\n", proxyAddress(d))
	if d.Proxy.Healthy {
		fmt.Fprintf(&b, "Proxy token captured: %t\n", d.Proxy.Health.TokenCaptured)
	}

	b.WriteString("\nTailscale:\n")
	switch {
	case d.TailErr != nil:
		fmt.Fprintf(&b, "unavailable: %v\n", d.TailErr)
	default:
		if d.Tailnet != "" {
			fmt.Fprintf(&b, "https://%s\n", d.Tailnet)
		}
		expected := "http://" + dsh.Loopback + ":" + strconv.Itoa(opts.proxyPort)
		switch {
		case d.NoServe:
			// Serve is deliberately not managed; do not report its state as a
			// fault of this run.
			b.WriteString("serve: not managed (--no-serve)\n")
		case d.Serve == expected:
			// Ours.
		case d.Serve == "":
			b.WriteString("serve: no handler is mounted at / (run `dsh-remote start`)\n")
		default:
			fmt.Fprintf(&b, "serve: / is mapped to %s, not the dsh-remote proxy at %s\n", d.Serve, expected)
		}
		if d.TailNote != "" {
			fmt.Fprintf(&b, "serve: %s\n", d.TailNote)
		}
	}

	// The two sections below are the incident follow-up: state plainly whether
	// remote access works, why it does not, and whether anything would fix it
	// without a human.
	b.WriteString("\nRemote access: ")
	if reachable(sit) {
		b.WriteString("AVAILABLE\n")
	} else {
		b.WriteString("UNAVAILABLE\n")
	}

	b.WriteString("\nReason:\n")
	b.WriteString(reasonLine(d, sit, opts))

	b.WriteString("\nService supervision:\n")
	b.WriteString(supervisionLines(d, sit))

	b.WriteString("\nSuggested action:\n")
	b.WriteString(suggestionLines(d, sit, opts))

	b.WriteString("\nDiagnostic log:\n")
	b.WriteString(logSummaryLines(d))

	b.WriteString("\nRemote DSH:\n")
	if d.State != nil && d.State.RemoteURL != "" {
		fmt.Fprintf(&b, "%s\n", d.State.RemoteURL)
	} else if d.Tailnet != "" {
		fmt.Fprintf(&b, "https://%s%s\n", d.Tailnet, proxy.BootstrapPath)
	} else {
		b.WriteString("unknown (Tailscale hostname not detected)\n")
	}
	if d.State != nil && d.State.LANURL != "" {
		b.WriteString("\nLocal network DSH (no Tailscale required):\n")
		fmt.Fprintf(&b, "%s\n", d.State.LANURL)
	}
	return b.String()
}

// dshStatusText renders the DSH section heading.
func dshStatusText(d diagnose) string {
	switch {
	case d.DSH.Listening && d.DSH.AuthChallenge:
		return "running"
	case d.DSH.Listening:
		return "running (unexpected response to an unauthenticated request)"
	case d.DSH.Alive:
		return "process alive but not listening on its port"
	default:
		return "not running"
	}
}

// dshPIDLines describes which DeepSeek Harness process the status refers to. It
// deliberately never prints a recorded pid as though it were the live one: after
// an unclean exit the recorded child can be long dead while another process
// holds the port, and conflating the two is what makes a report untrustworthy.
func dshPIDLines(d diagnose) []string {
	recorded := 0
	if d.State != nil {
		recorded = d.State.DSHPID
	}
	if recorded > 0 && d.DSH.Alive {
		return []string{fmt.Sprintf("DSH PID: %d (recorded by dsh-remote)", recorded)}
	}
	if recorded > 0 {
		line := fmt.Sprintf("DSH PID: %d is recorded but no longer alive (left over from an earlier run)", recorded)
		if d.DSH.PID > 0 {
			return []string{line, fmt.Sprintf("DSH PID: %d is the process actually holding the port", d.DSH.PID)}
		}
		if d.DSH.Listening {
			return []string{line, "DSH PID: the current port holder could not be identified"}
		}
		return []string{line}
	}
	if d.DSH.PID > 0 {
		return []string{fmt.Sprintf("DSH PID: %d (not recorded by a dsh-remote run)", d.DSH.PID)}
	}
	return []string{"DSH PID: unknown (not started by a recorded dsh-remote run)"}
}

// proxyStatusText renders the proxy section heading.
func proxyStatusText(d diagnose) string {
	switch {
	case d.Proxy.Healthy:
		return "running"
	case d.Proxy.Forbidden:
		return "running, but its health endpoint refused the request"
	case d.Proxy.Listening:
		return "listening but not answering as expected"
	default:
		return "stopped"
	}
}

// proxyNoteLines flags run state that no longer describes the running proxy.
func proxyNoteLines(d diagnose) []string {
	if d.State == nil || d.State.PID <= 0 || d.Proxy.Healthy {
		return nil
	}
	if process.Alive(d.State.PID) {
		return nil
	}
	return []string{fmt.Sprintf(
		"note: run state still names proxy pid %d, which is not alive; the state file is stale",
		d.State.PID)}
}

// supervisionLines reports whether systemd would bring the proxy back.
func supervisionLines(d diagnose, sit situation) string {
	sd := d.Supervision
	var b strings.Builder
	switch {
	case !sd.Supported:
		b.WriteString("systemd: not supported on this platform\n")
	case !sd.Installed:
		fmt.Fprintf(&b, "systemd unit: not installed (%s)\n", sd.UnitPath)
		b.WriteString("no automatic restart is configured\n")
	default:
		state := "inactive"
		if sd.Active {
			state = "active"
		}
		fmt.Fprintf(&b, "systemd unit: installed at %s\n", sd.UnitPath)
		fmt.Fprintf(&b, "state: %s\n", state)
		fmt.Fprintf(&b, "Restart=%s\n", orUnknown(sd.Restart))
		if sd.RestartCount != "" && sd.RestartCount != "0" {
			fmt.Fprintf(&b, "restarts so far: %s\n", sd.RestartCount)
		}
		if sd.ExecMainCode != "" && sd.ExecMainStatus != "" {
			fmt.Fprintf(&b, "last exit: %s (status %s)\n", sd.ExecMainCode, sd.ExecMainStatus)
		}
		if sd.Linger == "no" {
			b.WriteString("lingering: disabled (the service will not start after reboot without a login;\n")
			b.WriteString("           run `sudo loginctl enable-linger $USER`)\n")
		} else if sd.Linger == "yes" {
			b.WriteString("lingering: enabled\n")
		}
	}
	if sd.Err != nil {
		fmt.Fprintf(&b, "note: could not query systemd: %v\n", sd.Err)
	}
	if !sd.CanRecover() && sit != situationHealthy {
		b.WriteString("no automatic recovery is available for the current state\n")
	}
	return b.String()
}

// reasonLine explains the classified outage in one or two lines.
func reasonLine(d diagnose, sit situation, opts statusOptions) string {
	var b strings.Builder
	switch sit {
	case situationHealthy:
		b.WriteString("The proxy is listening and its upstream is reachable.\n")
	case situationProxyDown:
		fmt.Fprintf(&b, "The proxy is not listening on port %d.\n", opts.proxyPort)
		if d.DSH.Listening {
			b.WriteString("DeepSeek Harness is still running, so only the proxy is missing.\n")
		}
	case situationOrphaned:
		fmt.Fprintf(&b, "The proxy is gone, but DeepSeek Harness (pid %d) that dsh-remote launched is\n", d.DSH.PID)
		fmt.Fprintf(&b, "still holding port %d. Both access paths go through the proxy, so the remote\n", opts.dshPort)
		b.WriteString("URL is unreachable.\n")
		fmt.Fprintf(&b, "A plain restart would fail: port %d is already in use.\n", opts.dshPort)
	case situationPortTaken:
		fmt.Fprintf(&b, "The proxy is not listening, and port %d is held by a process dsh-remote did not\n", opts.dshPort)
		b.WriteString("launch.\n")
		b.WriteString("dsh-remote will not stop a process it does not own.\n")
	case situationProxyUnhealthy:
		fmt.Fprintf(&b, "Something is listening on port %d but the proxy health check did not\n", opts.proxyPort)
		b.WriteString("succeed, so remote access cannot be confirmed.\n")
		if d.Proxy.Err != nil {
			fmt.Fprintf(&b, "detail: %v\n", d.Proxy.Err)
		}
	case situationStopped:
		b.WriteString("Neither the proxy nor DeepSeek Harness is listening.\n")
	}
	if d.TailErr != nil && sit != situationHealthy {
		fmt.Fprintf(&b, "Tailscale is also unavailable: %v\n", d.TailErr)
	}
	return b.String()
}

// suggestionLines lists concrete next commands for the current situation.
func suggestionLines(d diagnose, sit situation, opts statusOptions) string {
	sd := d.Supervision
	var b strings.Builder
	switch sit {
	case situationHealthy:
		b.WriteString("None required.\n")
	case situationOrphaned:
		b.WriteString("Run this to clear the leftover DeepSeek Harness and start a supervised proxy:\n")
		b.WriteString("    dsh-remote reconcile\n")
		b.WriteString("or, if a systemd service is installed:\n")
		b.WriteString("    systemctl --user restart " + systemd.UnitName + "\n")
		fmt.Fprintf(&b, "If you prefer to do it by hand: stop pid %d and any DSH it left behind, then\n", d.State.PID)
		b.WriteString("start again.\n")
	case situationProxyDown:
		if sd.CanRecover() {
			b.WriteString("Restart the supervised service:\n")
			b.WriteString("    systemctl --user restart " + systemd.UnitName + "\n")
		} else if sd.Installed {
			fmt.Fprintf(&b, "The installed unit has Restart=%s, which will not recover every exit.\n", orUnknown(sd.Restart))
			b.WriteString("Reinstall it with a restart policy:\n")
			b.WriteString("    dsh-remote install --force\n")
		} else {
			b.WriteString("Install a supervised service so this cannot persist:\n")
			if lan := lanAddress(d); lan != "" {
				b.WriteString("    dsh-remote install --lan\n")
			} else {
				b.WriteString("    dsh-remote install\n")
			}
			b.WriteString("For unattended starts after reboot, also run:\n")
			b.WriteString("    sudo loginctl enable-linger $USER\n")
		}
	case situationPortTaken:
		fmt.Fprintf(&b, "Identify pid %d and free port %d, or run DeepSeek Harness elsewhere with\n", d.DSH.PID, opts.dshPort)
		b.WriteString("    dsh-remote start --dsh-port <port>\n")
		b.WriteString("Then run `dsh-remote status` again.\n")
	case situationStopped:
		if sd.Installed {
			b.WriteString("Start the supervised service:\n")
			b.WriteString("    systemctl --user start " + systemd.UnitName + "\n")
		} else {
			b.WriteString("Start it in the foreground:\n")
			b.WriteString("    dsh-remote start\n")
			b.WriteString("Or install it as a supervised service:\n")
			b.WriteString("    dsh-remote install\n")
		}
	case situationProxyUnhealthy:
		b.WriteString("Inspect the proxy log, then restart it:\n")
		b.WriteString("    systemctl --user restart " + systemd.UnitName + "\n")
		b.WriteString("If it is not managed by systemd, stop it and run `dsh-remote start` again.\n")
	}
	return b.String()
}

// logSummaryLines points at the persistent log and reports how recently it was
// written. The timestamp is the point: after a crash the log is the only place
// that says whether the proxy died a minute ago or has been down for hours.
func logSummaryLines(d diagnose) string {
	var b strings.Builder
	log := d.Log
	if log.Path == "" {
		b.WriteString("unavailable (the state directory could not be resolved)\n")
		return b.String()
	}
	if !log.Present {
		fmt.Fprintf(&b, "%s (no file yet; it is created on the next start)\n", log.Path)
		return b.String()
	}
	fmt.Fprintf(&b, "%s\n", log.Path)
	fmt.Fprintf(&b, "size: %s, last written: %s\n", humanBytes(log.Size), humanAge(log.Modified))
	if log.LastLine != "" {
		fmt.Fprintf(&b, "last entry: %s\n", log.LastLine)
	}
	return b.String()
}

// humanBytes renders a byte count compactly.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// humanAge renders how long ago a timestamp was.
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	age := time.Since(t)
	if age < 0 {
		age = 0
	}
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%s ago", age.Round(time.Second))
	case age < time.Hour:
		return fmt.Sprintf("%s ago", age.Round(time.Minute))
	case age < 48*time.Hour:
		return fmt.Sprintf("%s ago", age.Round(time.Hour))
	default:
		return fmt.Sprintf("%s (%s ago)", t.Format("2006-01-02 15:04:05"), age.Round(24*time.Hour))
	}
}

// lanAddress returns the LAN address recorded for the running proxy, if any.
func lanAddress(d diagnose) string {
	if d.State == nil {
		return ""
	}
	return d.State.LANAddr
}

func orUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
