package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeUnit plants a unit file in a temp config home and returns that home.
func writeUnit(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, UnitName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestExecStartReadsTheInstalledCommand covers the line the handoff decision
// rests on. A quoting mistake here would compare unequal to a perfectly valid
// invocation, or worse, compare equal to the wrong one -- and `start` would then
// drive a service configured differently from what was asked for.
func TestExecStartReadsTheInstalledCommand(t *testing.T) {
	// Shaped exactly as install renders it: a path with a space is quoted as a
	// whole, values are joined to their flag, and a literal % is doubled.
	home := writeUnit(t, `[Unit]
Description=dsh-remote: a stable /dsh URL for DeepSeek Harness

[Service]
Environment=PATH=/usr/bin
ExecStart="/opt/my tools/dsh-remote" start --lan --dsh-exec="/opt/dsh tools/dsh" --trusted-host=host%%20name

[Install]
WantedBy=default.target
`)
	client := &Client{ConfigHome: home}

	exe, args, err := client.ExecStart()
	if err != nil {
		t.Fatal(err)
	}
	if exe != "/opt/my tools/dsh-remote" {
		t.Fatalf("exe = %q, want the unquoted path", exe)
	}
	want := []string{"start", "--lan", "--dsh-exec=/opt/dsh tools/dsh", "--trusted-host=host%20name"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

// TestExecStartUsesTheLastAssignment matches systemd's own rule: a later
// ExecStart= replaces the list, so reading the first one would describe a
// service that is not the one running.
func TestExecStartUsesTheLastAssignment(t *testing.T) {
	home := writeUnit(t, "[Service]\nExecStart=/usr/bin/old start\nExecStart=/usr/bin/new start --lan\n")
	client := &Client{ConfigHome: home}

	exe, args, err := client.ExecStart()
	if err != nil {
		t.Fatal(err)
	}
	if exe != "/usr/bin/new" || !slices.Equal(args, []string{"start", "--lan"}) {
		t.Fatalf("ExecStart = %q %q, want the last assignment", exe, args)
	}
}

// TestExecStartRejectsAUnitWithoutOne keeps a malformed unit from being read as
// "matches anything".
func TestExecStartRejectsAUnitWithoutOne(t *testing.T) {
	client := &Client{ConfigHome: writeUnit(t, "[Unit]\nDescription=empty\n")}
	if _, _, err := client.ExecStart(); err == nil {
		t.Fatal("ExecStart accepted a unit with no ExecStart= line")
	}
}

// TestSplitCommand pins the subset of systemd quoting the installer writes and
// this package must read back.
func TestSplitCommand(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{name: "plain", in: "/bin/dsh start --lan", want: []string{"/bin/dsh", "start", "--lan"}},
		{name: "empty", in: "", want: nil},
		{name: "spaces collapse", in: "  /bin/dsh   start  ", want: []string{"/bin/dsh", "start"}},
		{name: "double quoted value", in: `--dsh-exec="/opt/my dsh"`, want: []string{"--dsh-exec=/opt/my dsh"}},
		{name: "single quoted value", in: `--dsh-exec='/opt/my dsh'`, want: []string{"--dsh-exec=/opt/my dsh"}},
		{name: "escaped quote", in: `--x="a\"b"`, want: []string{`--x=a"b`}},
		{name: "escaped percent pair", in: `--x=a%%b`, want: []string{"--x=a%b"}},
		{name: "empty quoted arg", in: `--x=""`, want: []string{"--x="}},
		{name: "unterminated", in: `--x="oops`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SplitCommand(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("SplitCommand(%q) = %q, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SplitCommand(%q): %v", tc.in, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("SplitCommand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestLifecycleVerbsCallSystemctl pins the exact argv. These commands replace
// "run systemctl yourself", so a wrong verb or a missing --user would be an
// invisible failure.
func TestLifecycleVerbsCallSystemctl(t *testing.T) {
	var got [][]string
	client := &Client{run: func(_ context.Context, argv []string) ([]byte, error) {
		got = append(got, argv)
		return nil, nil
	}}

	calls := []struct {
		verb string
		call func(context.Context) error
	}{
		{"start", client.Start},
		{"stop", client.Stop},
		{"restart", client.Restart},
	}
	for _, tc := range calls {
		got = nil
		if err := tc.call(context.Background()); err != nil {
			t.Fatalf("%s: %v", tc.verb, err)
		}
		want := []string{"--user", tc.verb, UnitName}
		if len(got) != 1 || !slices.Equal(got[0], want) {
			t.Fatalf("%s argv = %q, want %q", tc.verb, got, want)
		}
	}
}

// TestLifecycleFailureIsReported keeps a failed handoff from reading as a
// successful start: the caller has nothing else to go on.
func TestLifecycleFailureIsReported(t *testing.T) {
	client := &Client{run: func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("Failed to connect to bus: No data available")
	}}
	err := client.Start(context.Background())
	if err == nil {
		t.Fatal("Start reported success while systemctl failed")
	}
	if !strings.Contains(err.Error(), "Failed to connect to bus") || !strings.Contains(err.Error(), UnitName) {
		t.Fatalf("error should name the unit and the cause: %v", err)
	}
}
