package socket

import (
	"net"
	"os"
	"testing"
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
