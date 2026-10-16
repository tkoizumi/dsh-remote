package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// lifecycleTimeout bounds a start/stop/restart request. Stopping waits for the
// unit's own teardown -- the proxy restores Tailscale Serve and stops DeepSeek
// Harness before it exits -- so it needs far longer than a query.
const lifecycleTimeout = 45 * time.Second

// Start asks systemd to start the unit.
func (c *Client) Start(ctx context.Context) error { return c.lifecycle(ctx, "start") }

// Stop asks systemd to stop the unit.
func (c *Client) Stop(ctx context.Context) error { return c.lifecycle(ctx, "stop") }

// Restart asks systemd to restart the unit.
func (c *Client) Restart(ctx context.Context) error { return c.lifecycle(ctx, "restart") }

func (c *Client) lifecycle(ctx context.Context, verb string) error {
	if _, err := c.execTimeout(ctx, lifecycleTimeout, verb, UnitName); err != nil {
		return fmt.Errorf("systemctl --user %s %s: %w", verb, UnitName, err)
	}
	return nil
}

// ExecStart returns the executable and its arguments from the unit's ExecStart=
// line.
//
// A caller compares this with its own invocation to answer "is the installed
// service the thing I am being asked to start?". Only the last non-empty
// ExecStart= assignment is read, which is what systemd runs for the units
// dsh-remote writes (a single line, with no overrides).
func (c *Client) ExecStart() (string, []string, error) {
	path, err := c.UnitPath()
	if err != nil {
		return "", nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	command := ""
	for _, line := range strings.Split(string(data), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if ok && strings.TrimSpace(rest) != "" {
			command = strings.TrimSpace(rest)
		}
	}
	if command == "" {
		return "", nil, fmt.Errorf("%s has no ExecStart= line", path)
	}
	argv, err := SplitCommand(command)
	if err != nil {
		return "", nil, fmt.Errorf("parse ExecStart in %s: %w", path, err)
	}
	if len(argv) == 0 {
		return "", nil, fmt.Errorf("%s has an empty ExecStart= line", path)
	}
	return argv[0], argv[1:], nil
}

// SplitCommand splits one systemd command line into arguments, honouring the
// quoting that the installer writes: double or single quotes around a whole
// argument, and backslash escapes. A doubled percent is a literal one, because
// that is how the installer escapes a `%` that systemd would otherwise read as
// a specifier.
//
// This is the subset of systemd's syntax that dsh-remote generates and must be
// able to read back; it is not a general systemd parser.
func SplitCommand(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		started bool
		dquote  bool
		squote  bool
	)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\' && i+1 < len(s) && !squote:
			i++
			cur.WriteByte(s[i])
			started = true
		case ch == '"' && !squote:
			dquote = !dquote
			started = true
		case ch == '\'' && !dquote:
			squote = !squote
			started = true
		case (ch == ' ' || ch == '\t') && !dquote && !squote:
			if started {
				args = append(args, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteByte(ch)
			started = true
		}
	}
	if dquote || squote {
		return nil, errors.New("unterminated quote")
	}
	if started {
		args = append(args, cur.String())
	}
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "%%", "%")
	}
	return args, nil
}
