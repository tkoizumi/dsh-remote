// Package tailscale talks to the local `tailscale` CLI. It detects this node's
// MagicDNS name and manages exactly one Serve handler: the root path proxied to
// dsh-remote. Tailscale Funnel is never used.
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// commandTimeout bounds each tailscale invocation.
const commandTimeout = 20 * time.Second

// Client runs tailscale subcommands with a fixed executable path.
type Client struct {
	Bin string
}

// New resolves the tailscale executable, returning a clear error when it is
// absent. Nothing is ever installed automatically.
func New() (*Client, error) {
	path, err := exec.LookPath("tailscale")
	if err != nil {
		return nil, fmt.Errorf("tailscale not found in PATH: dsh-remote needs Tailscale to expose DeepSeek Harness to your tailnet (install and authenticate it first; see the README)")
	}
	return &Client{Bin: path}, nil
}

// NotRunningError reports that the local tailscaled is not up.
type NotRunningError struct {
	State string
}

func (e *NotRunningError) Error() string {
	return fmt.Sprintf("tailscale is not running (backend state %q); start it and authenticate, then retry", e.State)
}

type statusJSON struct {
	BackendState string `json:"BackendState"`
	Self         struct {
		DNSName string `json:"DNSName"`
	} `json:"Self"`
}

// DNSName returns this node's fully qualified MagicDNS name, without the
// trailing dot, for example ubuntu-server.tail6d6db9.ts.net.
func (c *Client) DNSName(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "status", "--json")
	if err != nil {
		return "", err
	}
	var status statusJSON
	if err := json.Unmarshal(out, &status); err != nil {
		return "", fmt.Errorf("parse `tailscale status --json`: %w", err)
	}
	if status.BackendState != "Running" {
		return "", &NotRunningError{State: status.BackendState}
	}
	name := strings.TrimSuffix(strings.TrimSpace(status.Self.DNSName), ".")
	if name == "" {
		return "", errors.New("tailscale reported no MagicDNS name for this node; run `tailscale status` and check that MagicDNS is enabled")
	}
	return name, nil
}

type serveStatus struct {
	Web map[string]struct {
		Handlers map[string]struct {
			Proxy string `json:"Proxy"`
			Text  string `json:"Text"`
		} `json:"Handlers"`
	} `json:"Web"`
}

// RootTarget returns the proxy target currently mounted at "/", or "" when no
// root handler is configured. It is used to record what we are about to
// replace so `stop` can put it back.
func (c *Client) RootTarget(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "serve", "status", "--json")
	if err != nil {
		return "", err
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	var status serveStatus
	if err := json.Unmarshal(trimmed, &status); err != nil {
		return "", fmt.Errorf("parse `tailscale serve status --json`: %w", err)
	}
	var hosts []string
	for host := range status.Web {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		if handler, ok := status.Web[host].Handlers["/"]; ok {
			if handler.Proxy != "" {
				return handler.Proxy, nil
			}
			return "", fmt.Errorf("tailscale serve already maps / on %s to something other than a proxy; dsh-remote will not overwrite it", host)
		}
	}
	return "", nil
}

// SetRootProxy mounts target at "/" for the default HTTPS port in the
// background, replacing any existing root handler. It never enables Funnel.
func (c *Client) SetRootProxy(ctx context.Context, target string) error {
	_, err := c.run(ctx, "serve", "--bg", "--yes", "--set-path=/", target)
	if err != nil {
		return err
	}
	return nil
}

// PermissionError marks a Serve configuration attempt that Tailscale refused
// because the current user is neither root nor the configured operator.
type PermissionError struct{ Err error }

func (e *PermissionError) Error() string {
	return fmt.Sprintf("tailscale refused to change the Serve configuration: %v", e.Err)
}

func (e *PermissionError) Unwrap() error { return e.Err }

// Guidance returns the one-time fix for a PermissionError.
func (e *PermissionError) Guidance() string {
	user := os.Getenv("USER")
	if user == "" {
		user = "$USER"
	}
	return "Tailscale Serve requires root or an operator. Enable it once with:\n" +
		"    sudo tailscale set --operator=" + user + "\n" +
		"then re-run `dsh-remote start`. Alternatively run dsh-remote with sudo."
}

// run executes the binary and returns stdout, translating the common
// permission failure into a typed error carrying remediation.
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "access denied") || strings.Contains(lower, "serve config denied") || strings.Contains(lower, "permission denied") {
			return nil, &PermissionError{Err: errors.New(firstLine(msg))}
		}
		return nil, fmt.Errorf("tailscale %s: %s", strings.Join(args, " "), firstLine(msg))
	}
	return stdout.Bytes(), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
