package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestQueryReadsInstalledUnit proves the diagnostics distinguish "installed"
// from "not installed" using the unit file, and then read the restart policy
// from systemctl. The restart policy is the part that decides whether the proxy
// comes back, so a unit whose ActiveState is fine but whose Restart is not must
// not be reported as recoverable.
func TestQueryReadsInstalledUnit(t *testing.T) {
	home := t.TempDir()
	unitDir := filepath.Join(home, "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, UnitName), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &Client{
		ConfigHome: home,
		run: func(_ context.Context, argv []string) ([]byte, error) {
			if len(argv) > 0 && argv[0] == "linger" {
				return []byte("yes\n"), nil
			}
			return []byte("ActiveState=active\nRestart=on-failure\nNRestarts=3\nExecMainStatus=1\nExecMainCode=exited\n"), nil
		},
	}

	state := client.Query(context.Background())
	if !state.Installed {
		t.Fatalf("Installed = false with a unit present at %s", state.UnitPath)
	}
	if !state.Active {
		t.Fatal("Active = false for ActiveState=active")
	}
	if state.Restart != "on-failure" {
		t.Fatalf("Restart = %q, want on-failure", state.Restart)
	}
	if state.RestartCount != "3" || state.ExecMainStatus != "1" || state.ExecMainCode != "exited" {
		t.Fatalf("exit details not parsed: %+v", state)
	}
	if state.Linger != "yes" {
		t.Fatalf("Linger = %q, want yes", state.Linger)
	}
	if !state.CanRecover() {
		t.Fatal("CanRecover = false for Restart=on-failure")
	}
	if state.Err != nil {
		t.Fatalf("unexpected error: %v", state.Err)
	}
}

// TestQueryMissingUnit covers the state the incident report found: no unit file
// at all. systemd answers `show` for a unit that does not exist with empty
// properties, so Installed must come from the file and not from that reply.
func TestQueryMissingUnit(t *testing.T) {
	client := &Client{
		ConfigHome: t.TempDir(),
		run: func(_ context.Context, argv []string) ([]byte, error) {
			if len(argv) > 0 && argv[0] == "linger" {
				return []byte("no\n"), nil
			}
			// What systemd prints for a unit that does not exist.
			return []byte("ActiveState=inactive\nRestart=\nNRestarts=0\nExecMainStatus=0\nExecMainCode=0\n"), nil
		},
	}

	state := client.Query(context.Background())
	if state.Installed {
		t.Fatalf("Installed = true with no unit file at %s", state.UnitPath)
	}
	if state.Active {
		t.Fatal("Active = true for an inactive unit")
	}
	if state.CanRecover() {
		t.Fatal("CanRecover = true with no unit installed")
	}
}

// TestCanRecoverRejectsRestartNo pins the distinction the README makes: an
// installed unit with Restart=no still cannot recover, and diagnostics must not
// promise otherwise.
func TestCanRecoverRejectsRestartNo(t *testing.T) {
	cases := map[string]bool{
		"always":      true,
		"on-failure":  true,
		"on-abnormal": true,
		"on-abort":    true,
		"no":          false,
		"":            false,
		"on-success":  false,
	}
	for restart, want := range cases {
		got := State{Installed: true, Restart: restart}.CanRecover()
		if got != want {
			t.Errorf("CanRecover(Restart=%q) = %t, want %t", restart, got, want)
		}
	}
}

// TestQuerySurfacesBusFailure proves a missing user bus degrades to a reported
// error instead of failing the whole status command.
func TestQuerySurfacesBusFailure(t *testing.T) {
	home := t.TempDir()
	unitDir := filepath.Join(home, "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, UnitName), []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &Client{
		ConfigHome: home,
		run: func(_ context.Context, argv []string) ([]byte, error) {
			if len(argv) > 0 && argv[0] == "linger" {
				return []byte("no\n"), nil
			}
			return nil, errors.New("Failed to connect to user scope bus via local transport")
		},
	}

	state := client.Query(context.Background())
	if state.Err == nil {
		t.Fatal("a failed systemctl query must be surfaced")
	}
	if !state.Installed {
		t.Fatal("Installed should still be decided by the unit file")
	}
	if state.CanRecover() {
		t.Fatal("CanRecover must not be true when the restart policy could not be read")
	}
}

// TestParseShow pins the property format.
func TestParseShow(t *testing.T) {
	props := parseShow([]byte("ActiveState=inactive\nRestart=\nunknown-line\nNRestarts=0\n"))
	if props["ActiveState"] != "inactive" {
		t.Fatalf("ActiveState = %q", props["ActiveState"])
	}
	if props["Restart"] != "" {
		t.Fatalf("Restart = %q, want empty", props["Restart"])
	}
	if _, ok := props["unknown-line"]; ok {
		t.Fatal("a line without = must be ignored")
	}
	if props["NRestarts"] != "0" {
		t.Fatalf("NRestarts = %q", props["NRestarts"])
	}
}
