// Package lima drives the `limactl` CLI on behalf of `dsh-remote vm`.
//
// It manages exactly one Lima instance: a persistent Linux VM that runs
// DeepSeek Harness and its own dsh-remote. Lima forwards the guest's loopback
// ports to the host's loopback, which is why a dsh-remote bound to 127.0.0.1
// inside the VM is reachable from a browser on the host with no extra wiring.
package lima

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// commandTimeout bounds each limactl invocation. Creating an instance
// downloads an image and runs provisioning, so it is deliberately generous.
const commandTimeout = 30 * time.Minute

// Client runs limactl subcommands with a fixed executable path.
type Client struct {
	Bin string
}

// ErrNotInstalled reports that limactl is absent, so a caller can offer to
// install Lima rather than failing outright.
var ErrNotInstalled = errors.New("limactl not found in PATH")

// Lookup resolves the limactl executable. Nothing is ever installed here.
func Lookup() (string, error) {
	path, err := exec.LookPath("limactl")
	if err != nil {
		return "", ErrNotInstalled
	}
	return path, nil
}

// New resolves the limactl executable, returning a clear error when it is
// absent. Nothing is ever installed automatically.
func New() (*Client, error) {
	path, err := Lookup()
	if err != nil {
		return nil, fmt.Errorf("%w: dsh-remote vm needs Lima to run DeepSeek Harness in a virtual machine (install it with `brew install lima` and try again)", err)
	}
	return &Client{Bin: path}, nil
}

// Instance is the subset of `limactl list --format json` that dsh-remote uses.
type Instance struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Dir    string `json:"dir"`
	VMType string `json:"vmType"`
	Arch   string `json:"arch"`
}

// Running reports whether Lima considers the instance up.
func (i Instance) Running() bool { return strings.EqualFold(i.Status, "running") }

// List returns every Lima instance. `limactl list --format json` emits one JSON
// object per line.
func (c *Client) List(ctx context.Context) ([]Instance, error) {
	out, err := c.run(ctx, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var instances []Instance
	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line == "null" {
			continue
		}
		var instance Instance
		if err := json.Unmarshal([]byte(line), &instance); err != nil {
			return nil, fmt.Errorf("parse `limactl list --format json`: %w", err)
		}
		if instance.Name != "" {
			instances = append(instances, instance)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return instances, nil
}

// Find returns the named instance, or nil when it does not exist.
func (c *Client) Find(ctx context.Context, name string) (*Instance, error) {
	instances, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range instances {
		if instances[i].Name == name {
			return &instances[i], nil
		}
	}
	return nil, nil
}

// Start creates the instance from configPath when it does not exist yet, or
// starts an existing one when configPath is empty.
func (c *Client) Start(ctx context.Context, name, configPath string) error {
	args := []string{"start", "--name", name, "--tty=false"}
	if configPath != "" {
		args = append(args, configPath)
	}
	_, err := c.run(ctx, args...)
	return err
}

// Stop shuts down the named instance.
func (c *Client) Stop(ctx context.Context, name string) error {
	_, err := c.run(ctx, "stop", name)
	return err
}

// Copy copies host paths into the guest. The target must be a guest reference
// such as "dsh:/tmp/dsh-remote".
func (c *Client) Copy(ctx context.Context, sources []string, target string) error {
	args := append([]string{"copy"}, sources...)
	args = append(args, target)
	_, err := c.run(ctx, args...)
	return err
}

// Guest runs a command inside the guest and returns its standard output. It
// starts the instance first when it is stopped.
func (c *Client) Guest(ctx context.Context, name string, argv ...string) (string, error) {
	args := append([]string{"shell", "--start", "--tty=false", name, "--"}, argv...)
	return c.run(ctx, args...)
}

// Interactive returns a command that opens a shell (or runs argv) in the guest
// with the caller's standard streams attached.
func (c *Client) Interactive(name string, argv ...string) *exec.Cmd {
	args := []string{"shell", "--start", name}
	if len(argv) > 0 {
		args = append(args, "--")
		args = append(args, argv...)
	}
	return exec.Command(c.Bin, args...)
}

// run executes limactl and returns stdout. On failure the error carries the
// captured stderr so the caller can show why Lima refused.
func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			detail = err.Error()
		}
		return stdout.String(), fmt.Errorf("limactl %s: %s", strings.Join(args, " "), detail)
	}
	return stdout.String(), nil
}

// Config describes the Lima instance dsh-remote creates for DeepSeek Harness.
type Config struct {
	// CPUs, Memory and Disk size the VM.
	CPUs   int
	Memory string
	Disk   string
	// Mount is a host directory shared writable into the guest at the same
	// path. Empty shares nothing and leaves the VM self-contained.
	Mount string
}

// Defaults is the instance shape used when the flags are not given. It mirrors
// examples/lima/dsh.yaml.
var Defaults = Config{CPUs: 4, Memory: "8GiB", Disk: "60GiB", Mount: "~/src"}

// provision installs the guest toolchain. Lima re-runs provisioning on every
// boot, so every step is guarded.
const provision = `#!/bin/bash
set -eux
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y curl git

if ! command -v node >/dev/null || [ "$(node -p 'process.versions.node.split(".")[0]')" -lt 20 ]; then
  curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
  apt-get install -y nodejs
fi

if ! command -v dsh >/dev/null; then
  npm install -g @deepseek-ai/dsh
fi

# Record the resolved dsh path so the host can pass it as --dsh-exec without
# needing a shell (and therefore quoting) over limactl shell.
install -d /usr/local/share/dsh-remote
command -v dsh > /usr/local/share/dsh-remote/dsh-exec
`

// YAML renders the Lima configuration for this instance.
func (c Config) YAML() string {
	var b strings.Builder
	b.WriteString("base: template:_images/ubuntu\n\n")
	fmt.Fprintf(&b, "cpus: %d\n", c.CPUs)
	fmt.Fprintf(&b, "memory: %s\n", c.Memory)
	fmt.Fprintf(&b, "disk: %s\n", c.Disk)
	b.WriteString("\n")
	if c.Mount != "" {
		b.WriteString("mounts:\n")
		fmt.Fprintf(&b, "  - location: %q\n", c.Mount)
		b.WriteString("    writable: true\n\n")
	}
	b.WriteString("provision:\n  - mode: system\n    script: |\n")
	for _, line := range strings.Split(strings.TrimRight(provision, "\n"), "\n") {
		fmt.Fprintf(&b, "      %s\n", line)
	}
	return b.String()
}
