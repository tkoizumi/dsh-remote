package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/tkoizumi/dsh-remote/internal/dsh"
	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/proxy"
	"github.com/tkoizumi/dsh-remote/internal/systemd"
)

// checkStatus is the outcome of one doctor check.
type checkStatus int

const (
	checkOK checkStatus = iota
	checkWarn
	checkFail
	checkSkip
)

func (s checkStatus) label() string {
	switch s {
	case checkOK:
		return "ok"
	case checkWarn:
		return "warn"
	case checkFail:
		return "FAIL"
	case checkSkip:
		return "skip"
	}
	return "?"
}

// check is one diagnostic line. Advice is printed only when it is non-empty.
type check struct {
	Name   string
	Status checkStatus
	Detail string
	Advice string
}

// errDoctorFailed signals an unhealthy verdict without printing a bare error.
var errDoctorFailed = errors.New("one or more checks failed")

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var opts statusOptions
	fs.IntVar(&opts.dshPort, "dsh-port", defaultDSHPort, "loopback port DeepSeek Harness listens on")
	fs.IntVar(&opts.proxyPort, "proxy-port", defaultProxyPort, "loopback port the stable proxy listens on")
	strict := fs.Bool("strict", false, "exit non-zero when any check fails")
	if err := fs.Parse(args); err != nil {
		return err
	}

	d := runDiagnose(context.Background(), opts)
	checks := buildChecks(d, opts)
	fmt.Print(renderChecks(checks, d, opts))

	if *strict && anyFailed(checks) {
		return errDoctorFailed
	}
	return nil
}

// anyFailed reports whether a check failed outright.
func anyFailed(checks []check) bool {
	for _, c := range checks {
		if c.Status == checkFail {
			return true
		}
	}
	return false
}

// buildChecks runs every end-to-end check against one diagnostics pass. Checks
// are ordered by dependency so the first failure explains the rest.
func buildChecks(d diagnose, opts statusOptions) []check {
	var checks []check
	checks = append(checks, check{
		Name:   "run state",
		Status: checkRunState(d),
		Detail: runStateDetail(d),
	})
	checks = append(checks, checkDSHCheck(d, opts)...)
	checks = append(checks, checkProxy(d, opts)...)
	checks = append(checks, checkBootstrap(d, opts))
	checks = append(checks, checkTailscale(d, opts))
	checks = append(checks, checkSupervision(d))
	checks = append(checks, checkOrphan(d, opts))
	return checks
}

func checkRunState(d diagnose) checkStatus {
	if d.State == nil {
		return checkSkip
	}
	// Stale state matters even while the proxy is healthy: it is what makes a
	// later `start` refuse to launch after the next unclean exit.
	if d.State.PID > 0 && !process.Alive(d.State.PID) {
		return checkWarn
	}
	if d.State.DSHPID > 0 && !d.DSH.Alive {
		return checkWarn
	}
	return checkOK
}

func runStateDetail(d diagnose) string {
	if d.State == nil {
		return "no dsh-remote run is recorded (nothing has started yet, or `stop` cleaned up)"
	}
	detail := fmt.Sprintf("recorded proxy pid %d, DSH pid %d, started %s",
		d.State.PID, d.State.DSHPID, d.State.StartedAt.Format("2006-01-02 15:04:05"))
	switch {
	case d.State.PID > 0 && !process.Alive(d.State.PID):
		detail += "; the recorded proxy pid is not alive, so this state is stale"
	case d.State.DSHPID > 0 && !d.DSH.Alive:
		detail += "; the recorded DSH pid is not alive, so this state is stale"
	}
	return detail
}

// checkDSHCheck covers the DeepSeek Harness process and its port.
func checkDSHCheck(d diagnose, opts statusOptions) []check {
	var checks []check
	switch {
	case d.DSH.Listening && d.DSH.AuthChallenge:
		checks = append(checks, check{
			Name:   "DSH port",
			Status: checkOK,
			Detail: fmt.Sprintf("something on port %d answers as DeepSeek Harness (pid %s)", opts.dshPort, pidText(d.DSH.PID)),
		})
	case d.DSH.Listening:
		checks = append(checks, check{
			Name:   "DSH port",
			Status: checkWarn,
			Detail: fmt.Sprintf("port %d is listening but the response did not look like DeepSeek Harness", opts.dshPort),
			Advice: "confirm what owns the port before starting dsh-remote",
		})
	case d.DSH.Alive:
		checks = append(checks, check{
			Name:   "DSH port",
			Status: checkFail,
			Detail: fmt.Sprintf("recorded DeepSeek Harness pid %d is alive but nothing is listening on port %d", d.State.DSHPID, opts.dshPort),
			Advice: "run `dsh-remote stop` to clear the stale run, then start again",
		})
	default:
		checks = append(checks, check{
			Name:   "DSH port",
			Status: checkSkip,
			Detail: fmt.Sprintf("nothing is listening on port %d", opts.dshPort),
		})
	}

	probe := check{
		Name: "DSH upstream",
	}
	switch {
	case d.DSH.StatusCode == 0:
		probe.Status = checkSkip
		probe.Detail = "not probed: nothing is listening"
	case d.DSH.AuthChallenge:
		probe.Status = checkOK
		if d.DSH.CookieIssued {
			probe.Detail = "reachable; authenticates and issues a session cookie (value withheld)"
		} else {
			probe.Detail = "reachable; requires authentication as expected"
		}
	case d.DSH.StatusCode >= 400:
		probe.Status = checkWarn
		probe.Detail = fmt.Sprintf("reachable but answered HTTP %d to an unauthenticated request", d.DSH.StatusCode)
	default:
		probe.Status = checkWarn
		probe.Detail = fmt.Sprintf("answered HTTP %d to an unauthenticated request", d.DSH.StatusCode)
	}
	checks = append(checks, probe)
	return checks
}

// checkProxy covers the stable proxy process and its health endpoint.
func checkProxy(d diagnose, opts statusOptions) []check {
	var checks []check
	switch {
	case d.Proxy.Healthy:
		checks = append(checks, check{
			Name:   "proxy HTTP",
			Status: checkOK,
			Detail: fmt.Sprintf("health endpoint on port %d answered ok (version %s)", opts.proxyPort, orUnknown(d.Proxy.Health.Version)),
		})
	case d.Proxy.Forbidden:
		checks = append(checks, check{
			Name:   "proxy HTTP",
			Status: checkFail,
			Detail: "the proxy health endpoint refused the loopback probe",
			Advice: "this is unexpected; report it with the proxy version",
		})
	case d.Proxy.Listening:
		checks = append(checks, check{
			Name:   "proxy HTTP",
			Status: checkFail,
			Detail: fmt.Sprintf("port %d is bound but health did not succeed: %v", opts.proxyPort, d.Proxy.Err),
			Advice: "restart the proxy; if supervised, `systemctl --user restart " + systemd.UnitName + "`",
		})
	default:
		checks = append(checks, check{
			Name:   "proxy HTTP",
			Status: checkFail,
			Detail: fmt.Sprintf("nothing is listening on port %d", opts.proxyPort),
			Advice: "run `dsh-remote start`, or install the supervised service with `dsh-remote install`",
		})
	}

	token := check{Name: "bootstrap token"}
	switch {
	case !d.Proxy.Healthy:
		token.Status = checkSkip
		token.Detail = "not probed: the proxy is not answering"
	case d.Proxy.Health.TokenCaptured:
		token.Status = checkOK
		token.Detail = "captured and held in memory only (never printed, never written to disk)"
	default:
		token.Status = checkFail
		token.Detail = "the proxy is running but has not captured an authentication token"
		token.Advice = "the /dsh URL cannot redirect yet; restart the proxy to re-read the token"
	}
	checks = append(checks, token)
	return checks
}

// checkBootstrap proves the whole path the browser takes, without following the
// redirect (which would carry the token).
func checkBootstrap(d diagnose, opts statusOptions) check {
	if !d.Proxy.Healthy {
		return check{Name: "end to end", Status: checkSkip, Detail: "not probed: the proxy is not answering"}
	}
	result := probeBootstrap(context.Background(), d.Proxy.Addr)
	switch {
	case result.RedirectsToToken:
		return check{
			Name:   "end to end",
			Status: checkOK,
			Detail: fmt.Sprintf("GET %s answers %s with a token-bound redirect (token value withheld)", proxy.BootstrapPath, result.Status),
		}
	case result.Status == "503 Service Unavailable":
		return check{
			Name:   "end to end",
			Status: checkFail,
			Detail: fmt.Sprintf("GET %s answers %s: no token has been captured yet", proxy.BootstrapPath, result.Status),
			Advice: "wait a moment and retry; if it persists, restart the proxy",
		}
	default:
		return check{
			Name:   "end to end",
			Status: checkFail,
			Detail: fmt.Sprintf("GET %s answered %s", proxy.BootstrapPath, result.Status),
			Advice: "the stable URL will not work; restart the proxy",
		}
	}
}

// checkTailscale confirms the tailnet identity and the Serve root mapping.
func checkTailscale(d diagnose, opts statusOptions) check {
	if d.TailErr != nil {
		return check{
			Name:   "Tailscale",
			Status: checkWarn,
			Detail: fmt.Sprintf("unavailable: %v", d.TailErr),
			Advice: "only the local network URL can work until Tailscale is up",
		}
	}
	expected := "http://" + dsh.Loopback + fmt.Sprintf(":%d", opts.proxyPort)
	switch {
	case d.NoServe:
		return check{Name: "Tailscale", Status: checkSkip, Detail: "Serve is not managed for this run (--no-serve)"}
	case d.Tailnet == "":
		return check{Name: "Tailscale", Status: checkWarn, Detail: "connected but no MagicDNS name was reported"}
	case d.Serve == expected:
		return check{Name: "Tailscale", Status: checkOK, Detail: fmt.Sprintf("Serve maps / on %s to %s", d.Tailnet, expected)}
	case d.Serve == "":
		return check{
			Name:   "Tailscale",
			Status: checkWarn,
			Detail: fmt.Sprintf("connected as %s but no handler is mounted at /", d.Tailnet),
			Advice: "run `dsh-remote start` to mount the proxy",
		}
	default:
		return check{
			Name:   "Tailscale",
			Status: checkWarn,
			Detail: fmt.Sprintf("Serve maps / to %s instead of %s", d.Serve, expected),
			Advice: "this is someone else's mapping; dsh-remote will not overwrite it silently",
		}
	}
}

// checkSupervision reports whether an outage would heal on its own.
func checkSupervision(d diagnose) check {
	sd := d.Supervision
	switch {
	case !sd.Supported:
		return check{Name: "supervision", Status: checkSkip, Detail: "not available on this platform"}
	case !sd.Installed:
		return check{
			Name:   "supervision",
			Status: checkWarn,
			Detail: fmt.Sprintf("no systemd unit at %s, so nothing restarts the proxy", sd.UnitPath),
			Advice: "run `dsh-remote install`, then `sudo loginctl enable-linger $USER` for reboot recovery",
		}
	case sd.Active:
		return check{
			Name:   "supervision",
			Status: checkOK,
			Detail: fmt.Sprintf("unit active, Restart=%s", orUnknown(sd.Restart)),
		}
	default:
		detail := "unit installed but not active"
		if !sd.CanRecover() {
			detail = fmt.Sprintf("unit installed with Restart=%s, which will not recover every exit", orUnknown(sd.Restart))
		}
		return check{
			Name:   "supervision",
			Status: checkWarn,
			Detail: detail,
			Advice: "run `systemctl --user start " + systemd.UnitName + "`",
		}
	}
}

// checkOrphan is the specific failure mode from the incident report: the proxy
// is gone but the DeepSeek Harness it launched still holds its port, so a plain
// restart collides with it. Presented as a distinct check because the fix
// differs from every other failure here.
func checkOrphan(d diagnose, opts statusOptions) check {
	if !d.DSH.Listening || !d.DSH.Ours || d.Proxy.Healthy {
		return check{Name: "orphan check", Status: checkOK, Detail: "no leftover DeepSeek Harness holding the port"}
	}
	return check{
		Name:   "orphan check",
		Status: checkFail,
		Detail: fmt.Sprintf("DeepSeek Harness pid %d still holds port %d while the proxy is gone", d.DSH.PID, opts.dshPort),
		Advice: "run `dsh-remote start`, which reclaims the port automatically, or `dsh-remote reconcile`",
	}
}

// renderChecks prints the check table and a verdict.
func renderChecks(checks []check, d diagnose, opts statusOptions) string {
	var b strings.Builder
	sit := classify(d)

	b.WriteString("\ndsh-remote doctor\n")
	fmt.Fprintf(&b, "DSH port %d  |  proxy port %d\n\n", opts.dshPort, opts.proxyPort)
	for _, c := range checks {
		fmt.Fprintf(&b, "  [%-4s] %-14s %s\n", c.Status.label(), c.Name, c.Detail)
		if c.Advice != "" {
			fmt.Fprintf(&b, "           %-14s -> %s\n", "", c.Advice)
		}
	}

	b.WriteString("\n")
	if reachable(sit) {
		fmt.Fprintf(&b, "Verdict: healthy - remote access is available at %s\n", remoteURLOf(d))
	} else {
		fmt.Fprintf(&b, "Verdict: %s - remote access is UNAVAILABLE\n", sit)
		b.WriteString(reasonLine(d, sit, opts))
		b.WriteString(suggestionLines(d, sit, opts))
	}
	return b.String()
}

// remoteURLOf returns the best-known browser URL, or a placeholder.
func remoteURLOf(d diagnose) string {
	if d.State != nil && d.State.RemoteURL != "" {
		return d.State.RemoteURL
	}
	if d.Tailnet != "" {
		return "https://" + d.Tailnet + proxy.BootstrapPath
	}
	return "http://127.0.0.1:" + fmt.Sprint(d.Proxy.Addr)
}

// pidText renders a pid for the detail column.
func pidText(pid int) string {
	if pid <= 0 {
		return "unknown"
	}
	return fmt.Sprint(pid)
}
