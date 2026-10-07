// Package dsh launches DeepSeek Harness in its `web` mode and captures the
// short-lived bootstrap token it prints at startup.
//
// DeepSeek Harness is never modified or forked: this package only runs the
// documented command line and reads its stdout. The generated token is kept in
// memory by the caller and is never written to disk.
package dsh

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultPackage is the published DeepSeek Harness package.
const DefaultPackage = "@deepseek-ai/dsh"

// Loopback is the only host this program ever binds or talks to directly.
const Loopback = "127.0.0.1"

// StartupToken is the parsed result of a DeepSeek Harness startup line.
type StartupToken struct {
	// Token is the value of the `token` query parameter.
	Token string
	// URL is the full URL exactly as DeepSeek Harness printed it.
	URL string
}

// ansiRE strips the SGR escape sequences a terminal-aware logger may inject.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// urlRE finds whitespace-delimited http(s) URLs. Parentheses, quotes and
// angle brackets terminate a match so `(LAN: http://...)` does not swallow the
// closing bracket.
var urlRE = regexp.MustCompile(`https?://[^\s"'<>()]+`)

// ParseStartupToken extracts the bootstrap token from text emitted by
// `dsh web`. It accepts the documented announcement:
//
//	dsh web: http://127.0.0.1:3080/?token=ykUIFiyl2GcfrTIaUAth_iT7l_yZZkKhjvEABM6WxAo
//
// and, more generally, any http(s) URL carrying a non-empty `token` query
// parameter. The token's own format is never assumed beyond "it is present in
// the query string": the parameter is read with net/url so any future shape
// keeps working. A canonical `dsh web:` line wins over an incidental URL
// elsewhere in the text.
func ParseStartupToken(text string) (StartupToken, bool) {
	text = ansiRE.ReplaceAllString(text, "")
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(trimmed), "dsh web:") {
			continue
		}
		if st, ok := tokenFromString(trimmed); ok {
			return st, true
		}
	}
	return tokenFromString(text)
}

// tokenFromString returns the first URL in s that carries a non-empty token.
func tokenFromString(s string) (StartupToken, bool) {
	for _, raw := range urlRE.FindAllString(s, -1) {
		raw = strings.TrimRight(raw, ".,;:")
		parsed, err := url.Parse(raw)
		if err != nil {
			continue
		}
		token := parsed.Query().Get("token")
		if token == "" {
			continue
		}
		return StartupToken{Token: token, URL: raw}, true
	}
	return StartupToken{}, false
}

// Spec describes how to launch DeepSeek Harness.
type Spec struct {
	// NPX is the npx executable name or path. Empty means "npx".
	NPX string
	// Package is the npm package to run through npx. Empty means DefaultPackage.
	Package string
	// Exec, when non-empty, is a `dsh` executable run directly instead of
	// consulting npx. It exists for users with a local install and for tests.
	Exec string
	// Port is the loopback TCP port DeepSeek Harness should listen on.
	Port int
	// TrustedHosts are extra authorities passed with --trusted-host. The
	// Tailscale DNS name belongs here: DeepSeek Harness's /api fence only
	// accepts loopback or explicitly trusted authorities.
	TrustedHosts []string
}

// Args returns the argument vector for the DeepSeek Harness `web` command.
func (s Spec) Args() []string {
	args := []string{"web", "--no-open", "--port", strconv.Itoa(s.Port)}
	for _, host := range s.TrustedHosts {
		if host == "" {
			continue
		}
		args = append(args, "--trusted-host", host)
	}
	return args
}

// Command builds the executable invocation for the spec. When Exec is empty
// the command goes through npx with --yes so a non-interactive service start
// (systemd) never blocks on the "Ok to proceed?" prompt.
func (s Spec) Command(ctx context.Context) *exec.Cmd {
	if s.Exec != "" {
		return exec.CommandContext(ctx, s.Exec, s.Args()...)
	}
	npx := s.NPX
	if npx == "" {
		npx = "npx"
	}
	pkg := s.Package
	if pkg == "" {
		pkg = DefaultPackage
	}
	args := append([]string{"--yes", pkg}, s.Args()...)
	return exec.CommandContext(ctx, npx, args...)
}

// Process is a running DeepSeek Harness child process.
type Process struct {
	cmd   *exec.Cmd
	done  chan struct{}
	token chan struct{}

	logMu sync.Mutex
	logw  io.Writer

	tokenMu sync.RWMutex
	startup StartupToken

	waitErr error
	once    sync.Once
}

// Start launches DeepSeek Harness with its output relayed to logw (which may be
// nil). The child gets its own process group so the whole npx -> node tree can
// be signalled together and never orphaned.
func Start(ctx context.Context, spec Spec, logw io.Writer) (*Process, error) {
	cmd := spec.Command(ctx)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Graceful cancellation: ask the group to stop, then let WaitDelay force
	// the issue rather than leaking a stuck node process.
	cmd.Cancel = func() error { return signalGroup(cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe DeepSeek Harness stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe DeepSeek Harness stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start DeepSeek Harness: %w", err)
	}

	p := &Process{cmd: cmd, done: make(chan struct{}), token: make(chan struct{}), logw: logw}

	var wg sync.WaitGroup
	wg.Add(2)
	go p.consume(stdout, &wg)
	go p.consume(stderr, &wg)
	go func() {
		wg.Wait()
		p.waitErr = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// consume relays one output stream line by line, redacting the token from the
// relayed copy so the credential is not needlessly echoed back to the user.
func (p *Process) consume(r io.Reader, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if st, ok := ParseStartupToken(line); ok {
			p.setToken(st)
		}
		p.writeLog(line)
	}
}

func (p *Process) setToken(st StartupToken) {
	p.tokenMu.Lock()
	first := p.startup.Token == ""
	p.startup = st
	p.tokenMu.Unlock()
	if first {
		p.once.Do(func() { close(p.token) })
	}
}

func (p *Process) writeLog(line string) {
	if p.logw == nil {
		return
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	fmt.Fprintln(p.logw, p.redact(line))
}

// redact removes the captured token from a relayed line.
func (p *Process) redact(line string) string {
	tok := p.Token().Token
	if tok == "" {
		return line
	}
	return strings.ReplaceAll(line, tok, "REDACTED")
}

// Token returns the most recently observed startup token, if any.
func (p *Process) Token() StartupToken {
	p.tokenMu.RLock()
	defer p.tokenMu.RUnlock()
	return p.startup
}

// TokenReady is closed once a token has been observed.
func (p *Process) TokenReady() <-chan struct{} { return p.token }

// Done is closed when the child process has fully exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err reports why the child exited; it is only valid after Done is closed.
func (p *Process) Err() error { return p.waitErr }

// PID returns the child's process id, or 0 before start.
func (p *Process) PID() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// WaitToken waits until a startup token is observed, the child exits, or the
// timeout elapses. The returned error explains which of those happened.
func (p *Process) WaitToken(timeout time.Duration) (StartupToken, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.token:
		return p.Token(), nil
	case <-p.done:
		return StartupToken{}, fmt.Errorf("DeepSeek Harness exited before printing a startup URL: %s", ExitReason(p.Err()))
	case <-timer.C:
		return StartupToken{}, fmt.Errorf("timed out after %s waiting for DeepSeek Harness to print its startup URL", timeout)
	}
}

// Stop asks the whole process group to terminate and escalates to SIGKILL if it
// does not exit within timeout. It is safe to call more than once.
func (p *Process) Stop(timeout time.Duration) error {
	if p.cmd.Process == nil {
		return nil
	}
	select {
	case <-p.done:
		return nil
	default:
	}
	_ = signalGroup(p.cmd.Process.Pid, syscall.SIGTERM)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
		_ = signalGroup(p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
		return nil
	}
}

func signalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	// Negating the pid targets the process group created by Setpgid.
	if err := syscall.Kill(-pid, sig); err != nil {
		return syscall.Kill(pid, sig)
	}
	return nil
}

// ExitReason renders a child exit for a human, including the exit status a
// service manager would log.
func ExitReason(err error) string {
	if err == nil {
		return "exited with status 0"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return fmt.Sprintf("was terminated by signal %s", status.Signal())
			}
			return fmt.Sprintf("exited with status %d", status.ExitStatus())
		}
	}
	if errors.Is(err, context.Canceled) {
		return "was cancelled"
	}
	return err.Error()
}

// LookupNpx resolves the npx executable, producing a clear error when it is
// missing rather than failing later at process start.
func LookupNpx(name string) (string, error) {
	if name == "" {
		name = "npx"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("npx not found in PATH: install Node.js/npm (npx is required to run %s)", DefaultPackage)
	}
	return path, nil
}

// LookupExec resolves an explicitly supplied dsh executable.
func LookupExec(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("dsh executable %q not found: %w", name, err)
	}
	return path, nil
}

// ProcessAlive reports whether a pid names a live process that we could signal.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}
