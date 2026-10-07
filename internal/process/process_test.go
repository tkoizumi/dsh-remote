package process

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPortAvailableDetectsBusyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if err := PortAvailable("127.0.0.1", port); err == nil {
		t.Fatalf("port %d is busy but PortAvailable reported it free", port)
	}
}

func TestPortAvailableAcceptsFreePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if err := PortAvailable("127.0.0.1", port); err != nil {
		t.Fatalf("PortAvailable: %v", err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	t.Setenv("DSH_REMOTE_STATE_FILE", filepath.Join(t.TempDir(), "nested", "state.json"))

	if state, err := LoadState(); err != nil || state != nil {
		t.Fatalf("LoadState on empty = (%v, %v), want (nil, nil)", state, err)
	}

	want := &State{
		PID:              1234,
		DSHPID:           1235,
		DSHAddr:          "http://127.0.0.1:3080",
		ProxyAddr:        "http://127.0.0.1:3081",
		TailnetHost:      "ubuntu-server.tail6d6db9.ts.net",
		RemoteURL:        "https://ubuntu-server.tail6d6db9.ts.net/dsh",
		PriorServeTarget: "http://127.0.0.1:3080",
		ServeConfigured:  true,
		StartedAt:        time.Now().Truncate(time.Second),
	}
	if err := SaveState(want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("LoadState returned nil after SaveState")
	}
	if got.PID != want.PID || got.DSHPID != want.DSHPID || got.RemoteURL != want.RemoteURL ||
		got.PriorServeTarget != want.PriorServeTarget || !got.ServeConfigured {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}

	if err := RemoveState(); err != nil {
		t.Fatal(err)
	}
	if state, err := LoadState(); err != nil || state != nil {
		t.Fatalf("LoadState after RemoveState = (%v, %v), want (nil, nil)", state, err)
	}
}

// TestStateFileNeverContainsToken is a guard on the security requirement: the
// token lives in memory only.
func TestStateFileNeverContainsToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("DSH_REMOTE_STATE_FILE", path)
	if err := SaveState(&State{PID: 1, DSHPID: 2, RemoteURL: "https://host/dsh"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "token") {
		t.Fatalf("state file mentions a token: %s", data)
	}
}

func TestAlive(t *testing.T) {
	if !Alive(1) {
		t.Skip("pid 1 not signalable in this environment")
	}
	if Alive(0) {
		t.Fatal("pid 0 should not be reported alive")
	}
	// A pid that cannot exist.
	if Alive(1 << 30) {
		t.Fatal("absurd pid reported alive")
	}
}
