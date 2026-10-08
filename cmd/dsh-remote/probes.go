package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/dsh"
	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/proxy"
	"github.com/tkoizumi/dsh-remote/internal/socket"
	"github.com/tkoizumi/dsh-remote/internal/systemd"
	"github.com/tkoizumi/dsh-remote/internal/tailscale"
)

// noServeEnv marks a supervised run that was installed with --no-serve, so
// diagnostics can tell "Serve is unmanaged on purpose" from "Serve is broken".
const noServeEnv = "DSH_REMOTE_NO_SERVE"

// proxyProbe is the observed state of the stable proxy.
type proxyProbe struct {
	Addr      string
	Listening bool
	// Health is populated when the health endpoint answered with valid JSON.
	Health proxy.Health
	// Healthy is true when the health endpoint answered as expected.
	Healthy bool
	// Forbidden is true when the health endpoint refused a non-loopback peer.
	Forbidden bool
	Err       error
}

// dshProbe is the observed state of DeepSeek Harness.
type dshProbe struct {
	Addr string
	// Listening is true when something accepts TCP connections on the port.
	Listening bool
	// PID is the owner of the port when it could be attributed.
	PID int
	// Alive is true when the pid recorded in the run state is still live.
	Alive bool
	// Ours is true when the port owner is the child dsh-remote recorded.
	Ours bool
	// Inspectable is false when the owner of the port could not be identified.
	Inspectable bool
	// StatusCode is the HTTP status of a loopback GET /, when one was made.
	StatusCode int
	// AuthChallenge is true when that response carried DeepSeek Harness's
	// authentication requirement, confirming the identity of the listener.
	AuthChallenge bool
	// CookieIssued is true when the response minted an authentication cookie.
	// Only this boolean is retained: no credential is ever copied or printed.
	CookieIssued bool
	// Err records why the HTTP probe failed, when it did.
	Err error
}

// diagnose is the single observation pass shared by `status` and `doctor`.
type diagnose struct {
	State    *process.State
	DSH      dshProbe
	Proxy    proxyProbe
	Sockets  map[string]socket.Listener
	Health   proxy.Health
	Healthy  bool
	Tailnet  string
	Serve    string
	TailErr  error
	TailNote string
	// NoServe records that Serve management was explicitly declined, so a Serve
	// mapping pointing elsewhere is intentional and is not reported as a fault.
	NoServe bool
	// Supervision is the systemd state, which decides whether the proxy comes
	// back without a human.
	Supervision systemd.State
}

// lanAddrOf reports the LAN address recorded in the run state, if any.
func lanAddrOf(state *process.State) string {
	if state == nil {
		return ""
	}
	return state.LANAddr
}

// runDiagnose performs every read-only probe exactly once so the two reporting
// commands cannot disagree with each other.
func runDiagnose(ctx context.Context, opts statusOptions) diagnose {
	d := diagnose{
		Sockets: make(map[string]socket.Listener),
		// An installed unit that was created with --no-serve records that intent
		// in the environment, so a `status` run inside the unit does not report
		// Serve as misconfigured when it is deliberately unmanaged.
		NoServe: opts.noServe || os.Getenv(noServeEnv) != "",
	}

	state, err := process.LoadState()
	if err == nil {
		d.State = state
	}
	dshAddr := "http://" + dsh.Loopback + ":" + strconv.Itoa(opts.dshPort)
	proxyAddr := "http://" + dsh.Loopback + ":" + strconv.Itoa(opts.proxyPort)
	if state != nil {
		if state.DSHAddr != "" {
			dshAddr = state.DSHAddr
		}
		if state.ProxyAddr != "" {
			proxyAddr = state.ProxyAddr
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	d.DSH = probeDSH(probeCtx, dshAddr, recordedDSHPID(state))

	// Attribute both ports so `status` can say "listening" only when the
	// expected process owns them, not merely when the port is occupied.
	d.Proxy = probeProxy(probeCtx, proxyAddr)
	d.Health, d.Healthy = d.Proxy.Health, d.Proxy.Healthy

	d.Sockets[addrKey(dshAddr)] = socket.Inspect(opts.dshPort, recordedDSHPID(state))
	d.Sockets[addrKey(proxyAddr)] = socket.Inspect(opts.proxyPort, 0)
	if lan := lanAddrOf(state); lan != "" {
		d.Sockets[lan] = socket.Inspect(opts.proxyPort, 0)
	}

	d.Tailnet, d.Serve, d.TailErr, d.TailNote = probeTailscale(ctx, state, opts.proxyPort)

	// Supervision is the difference between an outage that heals and one that
	// persists, so it is probed on every pass. It never touches systemd state.
	d.Supervision = systemd.New().Query(ctx)
	return d
}

// recordedDSHPID returns the DeepSeek Harness pid from the run state, or 0.
func recordedDSHPID(state *process.State) int {
	if state == nil {
		return 0
	}
	return state.DSHPID
}

// addrKey normalises an address for map lookups.
func addrKey(addr string) string {
	return strings.TrimSuffix(strings.TrimPrefix(addr, "http://"), "/")
}

// probeProxy asks the proxy's loopback health endpoint for JSON status.
func probeProxy(ctx context.Context, addr string) proxyProbe {
	probe := proxyProbe{Addr: addr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+proxy.HealthPath, nil)
	if err != nil {
		probe.Err = err
		return probe
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		probe.Err = err
		// A refused connection means nothing is bound; any other transport
		// error is reported as such.
		probe.Listening = false
		return probe
	}
	defer resp.Body.Close()
	probe.Listening = true
	if resp.StatusCode == http.StatusForbidden {
		probe.Forbidden = true
		return probe
	}
	if resp.StatusCode != http.StatusOK {
		probe.Err = fmt.Errorf("health endpoint answered %s", resp.Status)
		return probe
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&probe.Health); err != nil {
		probe.Err = fmt.Errorf("health endpoint returned malformed JSON: %w", err)
		return probe
	}
	probe.Healthy = probe.Health.Status == "ok"
	return probe
}

// bootstrapResult is the observed outcome of a GET on the stable /dsh path.
type bootstrapResult struct {
	// Status is the HTTP status line.
	Status string
	// RedirectsToToken is true when the response redirects to the token-bound
	// bootstrap URL. The target is deliberately not retained: it contains the
	// live authentication token.
	RedirectsToToken bool
}

// probeBootstrap asks the proxy for the browser-facing /dsh URL and stops at
// the redirect. The Location header is inspected only to learn whether it
// carries a token; its value is never copied, returned, or printed.
func probeBootstrap(ctx context.Context, proxyAddr string) bootstrapResult {
	var result bootstrapResult
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, proxyAddr+proxy.BootstrapPath, nil)
	if err != nil {
		result.Status = "request error"
		return result
	}
	client := &http.Client{
		// Never follow the redirect: following it would carry the token
		// downstream and could mint a real session as a side effect.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		result.Status = "unreachable"
		return result
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()
	result.Status = resp.Status
	if location := resp.Header.Get("Location"); strings.Contains(location, "token=") {
		result.RedirectsToToken = true
	}
	return result
}

// fetchHealth is the two-second convenience probe used by the VM command, which
// polls repeatedly and has no diagnostics context to carry.
func fetchHealth(addr string) (proxy.Health, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	probe := probeProxy(ctx, addr)
	return probe.Health, probe.Healthy
}

// probeDSH performs an unauthenticated GET / against DeepSeek Harness. The
// response is read only for its status line and headers: the body is discarded
// and no cookie or token value is ever retained, copied, or printed.
func probeDSH(ctx context.Context, addr string, recordedPID int) dshProbe {
	probe := dshProbe{Addr: addr}
	port := portOf(addr, 0)
	if port > 0 {
		owner := socket.Inspect(port, recordedPID)
		probe.Listening = owner.Listening
		probe.PID = owner.PID
		probe.Ours = owner.Ours
		probe.Inspectable = owner.Inspectable
	}
	probe.Alive = process.Alive(recordedPID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/", nil)
	if err != nil {
		probe.Err = err
		return probe
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		probe.Err = err
		return probe
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()
	probe.StatusCode = resp.StatusCode
	probe.CookieIssued = len(resp.Cookies()) > 0
	if resp.StatusCode == http.StatusUnauthorized {
		probe.AuthChallenge = true
	}
	// A bootstrap redirect means the authority is already trusted; that is the
	// healthy shape for a browser that has not exchanged its token yet.
	if location := resp.Header.Get("Location"); location != "" && strings.Contains(location, "token=") {
		probe.AuthChallenge = true
	}
	return probe
}

// probeTailscale reads the tailnet identity and the Serve root mapping. It only
// reads: `status` and `doctor` never mutate Serve configuration.
func probeTailscale(ctx context.Context, state *process.State, proxyPort int) (host, serve string, err error, note string) {
	ts, newErr := tailscale.New()
	if newErr != nil {
		return "", "", newErr, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	host = ""
	if state != nil {
		host = state.TailnetHost
	}
	if host == "" {
		detected, dnsErr := ts.DNSName(ctx)
		if dnsErr != nil {
			return "", "", dnsErr, ""
		}
		host = detected
	}
	target, targetErr := ts.RootTarget(ctx)
	if targetErr != nil {
		note = targetErr.Error()
		return host, "", nil, note
	}
	return host, target, nil, ""
}

// portOf extracts the port from an address, or returns fallback.
func portOf(addr string, fallback int) int {
	parsed, err := url.Parse(addr)
	if err != nil {
		return fallback
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port <= 0 {
		return fallback
	}
	return port
}
