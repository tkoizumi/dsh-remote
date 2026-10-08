package lima

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLookupReportsNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Lookup(); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Lookup err = %v, want ErrNotInstalled", err)
	}
	if _, err := New(); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("New err = %v, want it to wrap ErrNotInstalled", err)
	}
}

func TestLookupFindsBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "limactl")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got, err := Lookup()
	if err != nil {
		t.Fatal(err)
	}
	if got != bin {
		t.Fatalf("Lookup = %q, want %q", got, bin)
	}
}

// fakeCLI writes a limactl stand-in that logs each argument on its own line and
// then runs body. It returns the client and the log path.
func fakeCLI(t *testing.T, body string) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args.log")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> '" + logPath + "'\n" + body
	path := filepath.Join(dir, "limactl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Client{Bin: path}, logPath
}

func loggedArgs(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read arg log: %v", err)
	}
	return strings.Fields(string(data))
}

func TestListParsesJSONLines(t *testing.T) {
	client, _ := fakeCLI(t, `printf '%s\n' '{"name":"dsh","status":"Running"}' '{"name":"other","status":"Stopped"}'`)
	instances, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("got %d instances, want 2: %+v", len(instances), instances)
	}
	if instances[0].Name != "dsh" || !instances[0].Running() {
		t.Fatalf("instances[0] = %+v, want running dsh", instances[0])
	}
	if instances[1].Name != "other" || instances[1].Running() {
		t.Fatalf("instances[1] = %+v, want stopped other", instances[1])
	}
}

func TestListIgnoresBlankAndNullLines(t *testing.T) {
	client, _ := fakeCLI(t, `printf '%s\n' '' 'null' '{"name":"dsh","status":"running"}'`)
	instances, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].Name != "dsh" {
		t.Fatalf("got %+v, want one dsh instance", instances)
	}
	if !instances[0].Running() {
		t.Fatalf("status matching should be case-insensitive: %+v", instances[0])
	}
}

func TestFindMissingInstanceReturnsNil(t *testing.T) {
	client, _ := fakeCLI(t, `printf '%s\n' '{"name":"other","status":"Running"}'`)
	inst, err := client.Find(context.Background(), "dsh")
	if err != nil {
		t.Fatal(err)
	}
	if inst != nil {
		t.Fatalf("got %+v, want nil", inst)
	}
}

func TestStartPassesNameAndConfig(t *testing.T) {
	client, logPath := fakeCLI(t, "")
	if err := client.Start(context.Background(), "dsh", "/tmp/lima.yaml"); err != nil {
		t.Fatal(err)
	}
	want := []string{"start", "--name", "dsh", "--tty=false", "/tmp/lima.yaml"}
	if got := loggedArgs(t, logPath); !equal(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestStartExistingInstanceOmitsConfig(t *testing.T) {
	client, logPath := fakeCLI(t, "")
	if err := client.Start(context.Background(), "dsh", ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"start", "--name", "dsh", "--tty=false"}
	if got := loggedArgs(t, logPath); !equal(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestGuestUsesDashDashForCommandFlags(t *testing.T) {
	client, logPath := fakeCLI(t, "")
	if _, err := client.Guest(context.Background(), "dsh", "systemctl", "--user", "is-active", "dsh-remote"); err != nil {
		t.Fatal(err)
	}
	want := []string{"shell", "--start", "--tty=false", "dsh", "--", "systemctl", "--user", "is-active", "dsh-remote"}
	if got := loggedArgs(t, logPath); !equal(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestCopyUsesGuestReference(t *testing.T) {
	client, logPath := fakeCLI(t, "")
	if err := client.Copy(context.Background(), []string{"/tmp/dsh-remote"}, "dsh:/tmp/dsh-remote"); err != nil {
		t.Fatal(err)
	}
	want := []string{"copy", "/tmp/dsh-remote", "dsh:/tmp/dsh-remote"}
	if got := loggedArgs(t, logPath); !equal(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestRunSurfacesStderr(t *testing.T) {
	client, _ := fakeCLI(t, "echo 'lima exploded' >&2\nexit 1\n")
	_, err := client.List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lima exploded") {
		t.Fatalf("err = %v, want it to carry stderr", err)
	}
}

func TestConfigYAMLMatchesExampleShape(t *testing.T) {
	yaml := Defaults.YAML()
	for _, want := range []string{
		"base: template:_images/ubuntu",
		"cpus: 4",
		"memory: 8GiB",
		"disk: 60GiB",
		"location: \"~/src\"",
		"writable: true",
		"provision:",
		"nodejs",
		"@deepseek-ai/dsh",
		"/usr/local/share/dsh-remote/dsh-exec",
	} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("YAML is missing %q:\n%s", want, yaml)
		}
	}
}

func TestConfigYAMLOmitsMountsWhenEmpty(t *testing.T) {
	cfg := Defaults
	cfg.Mount = ""
	if strings.Contains(cfg.YAML(), "mounts:") {
		t.Fatalf("empty mount should omit the mounts block:\n%s", cfg.YAML())
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
