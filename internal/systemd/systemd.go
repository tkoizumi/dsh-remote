// Package systemd reports whether dsh-remote is supervised by a systemd unit.
//
// It only reads state. The `install` command owns writing units, so this package
// never enables, starts, or edits anything; it exists so `status` and `doctor`
// can explain why an unattended deployment will not recover on its own.
package systemd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// UnitName is the unit both the installer and the diagnostics use.
const UnitName = "dsh-remote.service"

// commandTimeout bounds each systemctl invocation. A missing or hung user bus
// must not stall `status`.
const commandTimeout = 5 * time.Second

// State is the observed supervision state.
type State struct {
	// Supported is false on platforms with no systemd, where the rest of the
	// fields are meaningless.
	Supported bool
	// UnitPath is the user unit file that was looked for.
	UnitPath string
	// Installed is true when that file exists.
	Installed bool
	// Active is true when systemd reports the unit as active.
	Active bool
	// Restart is the configured Restart= value, for example "on-failure".
	Restart string
	// RestartCount is how many times systemd has restarted the unit.
	RestartCount string
	// ExecMainStatus is the last exit status systemd recorded.
	ExecMainStatus string
	// ExecMainCode classifies that exit, for example "exited" or "killed".
	ExecMainCode string
	// Linger is the loginctl lingering state ("yes"/"no") for this user, or ""
	// when it could not be read. Linger decides whether a user service starts
	// without an interactive login.
	Linger string
	// Err records why the query failed, when it did.
	Err error
}

// Client runs systemctl with a fixed binary path and an overridable unit
// location, so the query logic is testable without a systemd bus.
type Client struct {
	// Bin is the systemctl executable, or "systemctl" to resolve from PATH.
	Bin string
	// ConfigHome overrides the base of the user unit directory (normally
	// os.UserConfigDir()).
	ConfigHome string
	// run overrides command execution in tests.
	run func(ctx context.Context, argv []string) ([]byte, error)
}

// New builds a Client that queries systemctl on PATH.
func New() *Client { return &Client{Bin: "systemctl"} }

// UnitPath returns the user unit file location.
func (c *Client) UnitPath() (string, error) {
	base := c.ConfigHome
	if base == "" {
		var err error
		base, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(base, "systemd", "user", UnitName), nil
}

// Query reads the current supervision state. A nil error in the result does not
// mean the unit is healthy: it means the question could be answered.
func (c *Client) Query(ctx context.Context) State {
	state := State{Supported: runtime.GOOS == "linux"}

	path, err := c.UnitPath()
	if err != nil {
		state.Err = err
		return state
	}
	state.UnitPath = path
	if _, err := os.Stat(path); err == nil {
		state.Installed = true
	}

	if !state.Supported {
		return state
	}

	// `show` answers for a unit that does not exist and reports the properties
	// as empty, which is why Installed is decided by the unit file above and
	// not by this call. The restart policy lives here because an installed unit
	// with Restart=no still cannot recover.
	if out, err := c.exec(ctx, "show", UnitName,
		"--property=ActiveState,Restart,RestartUSec,NRestarts,ExecMainStatus,ExecMainCode"); err != nil {
		if state.Err == nil {
			state.Err = err
		}
	} else {
		props := parseShow(out)
		state.Active = props["ActiveState"] == "active"
		state.Restart = props["Restart"]
		state.RestartCount = props["NRestarts"]
		state.ExecMainStatus = props["ExecMainStatus"]
		state.ExecMainCode = props["ExecMainCode"]
	}

	// Lingering is what makes a *user* service start at boot with nobody logged
	// in. `loginctl` is optional: its absence is not a supervision failure.
	if out, err := c.execLinger(ctx); err == nil {
		state.Linger = strings.TrimSpace(string(out))
	}
	return state
}

// CanRecover reports whether this state would bring the proxy back after an
// unexpected exit.
func (s State) CanRecover() bool {
	if !s.Installed {
		return false
	}
	switch s.Restart {
	case "always", "on-failure", "on-abnormal", "on-abort":
		return true
	default:
		return false
	}
}

func (c *Client) exec(ctx context.Context, args ...string) ([]byte, error) {
	argv := append([]string{"--user"}, args...)
	if c.run != nil {
		return c.run(ctx, argv)
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	bin := c.Bin
	if bin == "" {
		bin = "systemctl"
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(firstLine(msg))
	}
	return stdout.Bytes(), nil
}

// execLinger asks loginctl whether this user's services survive logout.
func (c *Client) execLinger(ctx context.Context) ([]byte, error) {
	if c.run != nil {
		return c.run(ctx, []string{"linger"})
	}
	path, err := exec.LookPath("loginctl")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "show-user", lingerUser(), "--property=Linger", "--value")
	return cmd.Output()
}

// lingerUser names the user whose linger setting matters. `loginctl show-user`
// accepts a name, so this mirrors the operator-grant lookup in the tailscale
// package: $USER first, then the /etc/passwd entry.
func lingerUser() string {
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	if current, err := user.Current(); err == nil && current.Username != "" {
		return current.Username
	}
	return "root"
}

// parseShow turns `systemctl show` output into a property map.
func parseShow(out []byte) map[string]string {
	props := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		props[key] = value
	}
	return props
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
