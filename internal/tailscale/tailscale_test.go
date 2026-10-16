package tailscale

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLI writes a shell script that answers the subcommands the Client uses.
func fakeCLI(t *testing.T, script string) *Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tailscale")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Client{Bin: path}
}

func TestDNSNameStripsTrailingDot(t *testing.T) {
	client := fakeCLI(t, `echo '{"BackendState":"Running","Self":{"DNSName":"ubuntu-server.tail6d6db9.ts.net."}}'`)
	got, err := client.DNSName(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "ubuntu-server.tail6d6db9.ts.net" {
		t.Fatalf("DNSName = %q", got)
	}
}

func TestDNSNameReportsStoppedBackend(t *testing.T) {
	client := fakeCLI(t, `echo '{"BackendState":"Stopped","Self":{"DNSName":"host.ts.net."}}'`)
	_, err := client.DNSName(context.Background())
	var notRunning *NotRunningError
	if !errors.As(err, &notRunning) {
		t.Fatalf("err = %v, want NotRunningError", err)
	}
}

func TestRootTargetReadsExistingMapping(t *testing.T) {
	client := fakeCLI(t, `echo '{"Web":{"ubuntu-server.tail6d6db9.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3080"}}}}}'`)
	got, err := client.RootTarget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:3080" {
		t.Fatalf("RootTarget = %q, want http://127.0.0.1:3080", got)
	}
}

func TestRootTargetHandlesEmptyConfig(t *testing.T) {
	for name, output := range map[string]string{
		"empty object": `echo '{}'`,
		"null":         `echo 'null'`,
		"no web":       `echo '{"Web":{}}'`,
		"blank":        `true`,
	} {
		t.Run(name, func(t *testing.T) {
			client := fakeCLI(t, output)
			got, err := client.RootTarget(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != "" {
				t.Fatalf("RootTarget = %q, want empty", got)
			}
		})
	}
}

func TestPermissionDeniedIsTyped(t *testing.T) {
	client := fakeCLI(t, `echo "sending serve config: Access denied: serve config denied" >&2; exit 1`)
	err := client.SetRootProxy(context.Background(), "http://127.0.0.1:3081")
	var perm *PermissionError
	if !errors.As(err, &perm) {
		t.Fatalf("err = %v, want PermissionError", err)
	}
	if !strings.Contains(perm.Guidance(), "tailscale set --operator") {
		t.Fatalf("guidance = %q, want the operator fix", perm.Guidance())
	}
}

func TestSetRootProxyPassesExpectedArgs(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	client := fakeCLI(t, `echo "$@" > `+argsFile)
	if err := client.SetRootProxy(context.Background(), "http://127.0.0.1:3081"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(data))
	want := "serve --bg --yes --set-path=/ http://127.0.0.1:3081"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

// TestServeTargetReadsExtraPort covers the second HTTPS port an extra dashboard
// uses, and proves the default-port reader does not mistake it for the root.
func TestServeTargetReadsExtraPort(t *testing.T) {
	client := fakeCLI(t, `echo '{"Web":{"host.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:9119"}}},"host.ts.net":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3081"}}}}}'`)

	got, err := client.ServeTarget(context.Background(), 8443, "/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:9119" {
		t.Fatalf("ServeTarget(8443) = %q, want http://127.0.0.1:9119", got)
	}

	root, err := client.RootTarget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if root != "http://127.0.0.1:3081" {
		t.Fatalf("RootTarget = %q, want the 443 handler", root)
	}

	if empty, err := client.ServeTarget(context.Background(), 10000, "/"); err != nil || empty != "" {
		t.Fatalf("ServeTarget(10000) = (%q, %v), want empty", empty, err)
	}
}

// TestServeTargetTreatsUnlabelledHostAs443 matches how `serve status --json`
// omits the port for the default HTTPS listener.
func TestServeTargetTreatsUnlabelledHostAs443(t *testing.T) {
	client := fakeCLI(t, `echo '{"Web":{"host.ts.net":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:3081"}}}}}'`)
	got, err := client.ServeTarget(context.Background(), 443, "/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:3081" {
		t.Fatalf("ServeTarget(443) = %q, want http://127.0.0.1:3081", got)
	}
}

func TestSetServeProxyPassesExpectedArgs(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	client := fakeCLI(t, `echo "$@" > `+argsFile)
	if err := client.SetServeProxy(context.Background(), 8443, "/", "http://127.0.0.1:9119"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(data))
	want := "serve --bg --yes --https=8443 --set-path=/ http://127.0.0.1:9119"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestClearServePassesExpectedArgs(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	client := fakeCLI(t, `echo "$@" > `+argsFile)
	if err := client.ClearServe(context.Background(), 8443, "/"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(data))
	want := "serve --https=8443 --set-path=/ off"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}
