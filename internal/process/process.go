// Package process holds the small amount of local bookkeeping dsh-remote
// needs: loopback port checks and the on-disk run state shared by
// start/status/stop/qr.
//
// The state file deliberately never contains the DeepSeek Harness token.
package process

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// PortAvailable reports whether host:port can be bound right now. It binds and
// immediately releases, which is the standard best-effort check.
func PortAvailable(host string, port int) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("port %d is not available on %s: %w", port, host, err)
	}
	return ln.Close()
}

// State is the persisted run state. It is small, human-readable, and contains
// no credentials.
type State struct {
	PID              int       `json:"pid"`
	DSHPID           int       `json:"dshPid"`
	DSHAddr          string    `json:"dshAddr"`
	ProxyAddr        string    `json:"proxyAddr"`
	TailnetHost      string    `json:"tailnetHost,omitempty"`
	RemoteURL        string    `json:"remoteUrl,omitempty"`
	LANAddr          string    `json:"lanAddr,omitempty"`
	LANURL           string    `json:"lanUrl,omitempty"`
	PriorServeTarget string    `json:"priorServeTarget,omitempty"`
	ServeConfigured  bool      `json:"serveConfigured,omitempty"`
	Version          string    `json:"version,omitempty"`
	StartedAt        time.Time `json:"startedAt"`
}

// StatePath returns the state file location, honouring XDG_STATE_HOME and
// DSH_REMOTE_STATE_FILE overrides.
func StatePath() (string, error) {
	if override := os.Getenv("DSH_REMOTE_STATE_FILE"); override != "" {
		return override, nil
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "dsh-remote", "state.json"), nil
}

// LoadState reads the state file. A missing file yields (nil, nil) so callers
// can treat "never started" and "stopped" identically.
func LoadState() (*State, error) {
	path, err := StatePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file %s: %w", path, err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", path, err)
	}
	return &state, nil
}

// SaveState writes the state file atomically with owner-only permissions.
func SaveState(state *State) error {
	path, err := StatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}
	return os.Rename(tmp, path)
}

// RemoveState deletes the state file, ignoring an already-absent file.
func RemoveState() error {
	path, err := StatePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Alive reports whether the given pid names a live process.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// Terminate asks a pid to stop, escalating to SIGKILL after timeout. It is used
// by `stop` to reach a start process (which in turn stops DeepSeek Harness).
func Terminate(pid int, timeout time.Duration) error {
	if !Alive(pid) {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !Alive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return nil
}
