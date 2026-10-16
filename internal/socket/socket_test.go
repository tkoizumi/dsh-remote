package socket

import (
	"net"
	"os"
	"testing"
	"time"
)

// TestInspectAttributesOwnListener proves the attribution path end to end: a
// listener created in this process must be reported as listening and owned by
// this pid, which is what lets dsh-remote recognise its own DeepSeek Harness
// child and refuse to touch anything else.
func TestInspectAttributesOwnListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	got := Inspect(port, os.Getpid())
	if !got.Listening {
		t.Fatalf("Inspect(%d) reported not listening while this process holds it", port)
	}
	if !got.Inspectable {
		t.Fatalf("Inspect(%d) could not attribute the owner; /proc is unreadable here", port)
	}
	if got.PID != os.Getpid() {
		t.Fatalf("Inspect(%d).PID = %d, want %d", port, got.PID, os.Getpid())
	}
	if !got.Ours {
		t.Fatalf("Inspect(%d).Ours = false, want true for the recorded pid", port)
	}
}

// TestInspectForeignListener proves a holder that is not the recorded child is
// never labelled ours, which is the guard that stops dsh-remote from killing a
// DeepSeek Harness the user started by hand.
func TestInspectForeignListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	got := Inspect(port, os.Getpid()+100000)
	if !got.Listening {
		t.Fatalf("Inspect(%d) reported not listening", port)
	}
	if got.Ours {
		t.Fatalf("Inspect(%d).Ours = true for an unrelated recorded pid", port)
	}
}

// TestInspectFreePort covers the ordinary "nothing is running" case.
func TestInspectFreePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if got := Inspect(port, 0); got.Listening {
		t.Fatalf("Inspect(%d) reported listening on a closed port", port)
	}
}

// TestListeningIsTheCheapPortProbe covers the polling half of the API. The
// reclaim path uses it to wait out the kernel's asynchronous socket teardown,
// where an instant read can still show a just-killed listener with no owner, so
// it must answer both while the port is held and once it has been released.
func TestListeningIsTheCheapPortProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	if !Listening(port) {
		t.Fatalf("Listening(%d) = false while this process holds it", port)
	}
	_ = ln.Close()

	deadline := time.Now().Add(2 * time.Second)
	for Listening(port) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if Listening(port) {
		t.Fatalf("Listening(%d) = true after the listener was closed", port)
	}
}

// TestIsOursRecognisesTheGroupBehindTheShim is the regression guard for the
// incident: dsh-remote records the npx shim's pid, the port is held by the node
// child the shim spawned, and the only thing tying them together is the process
// group. Matching on the pid alone called that orphan a stranger, so every
// supervised restart failed on the port conflict.
func TestIsOursRecognisesTheGroupBehindTheShim(t *testing.T) {
	const (
		shim  = 7000
		child = 7001
	)

	cases := []struct {
		name     string
		pid      int
		pgid     int
		recorded int
		want     bool
	}{
		{"the recorded process itself", shim, shim, shim, true},
		{"child in the recorded process group", child, shim, shim, true},
		{"child in someone else's group", child, 7002, shim, false},
		{"owner group unknown", child, 0, shim, false},
		{"no recorded run", child, shim, 0, false},
		{"no owner identified", 0, 0, shim, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOurs(tc.pid, tc.pgid, tc.recorded); got != tc.want {
				t.Fatalf("isOurs(pid=%d, pgid=%d, recorded=%d) = %t, want %t",
					tc.pid, tc.pgid, tc.recorded, got, tc.want)
			}
		})
	}
}
