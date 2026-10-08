package socket

import (
	"net"
	"os"
	"path/filepath"
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

// TestInodeFromNetTCPParsesListenEntry pins the /proc/net/tcp format assumptions
// with a fixture, so a kernel-format change surfaces here rather than as a
// silently unattributable port.
func TestInodeFromNetTCPParsesListenEntry(t *testing.T) {
	fixture := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0C34 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 987654 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 00000000:0000 01 00000000:00000000 00:00000000 00000000  1000        0 111111 1 0000000000000000 100 0 0 10 0
`
	path := filepath.Join(t.TempDir(), "tcp")
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	// 0x0C34 = 3124 in LISTEN state.
	if inode, ok := inodeFromNetTCP(path, "0C34"); !ok || inode != 987654 {
		t.Fatalf("inodeFromNetTCP(listen) = (%d, %t), want (987654, true)", inode, ok)
	}
	// 0x1F90 = 8080 is ESTABLISHED, so it must not be attributed as a listener.
	if inode, ok := inodeFromNetTCP(path, "1F90"); ok {
		t.Fatalf("inodeFromNetTCP(established) = (%d, true), want not found", inode)
	}
	if inode, ok := inodeFromNetTCP(path, "0BB8"); ok {
		t.Fatalf("inodeFromNetTCP(absent) = (%d, true), want not found", inode)
	}
}

// TestInspectUnreadableOwnerIsUnattributable pins the safety property the
// preflight depends on: when the port is provably held but the owner cannot be
// identified, Inspect reports the port as held, PID 0, and not inspectable. The
// caller then refuses to touch it instead of guessing.
func TestInspectUnreadableOwnerIsUnattributable(t *testing.T) {
	root := t.TempDir()
	// Claim port 3124 is held (0C34, LISTEN) with inode 987654.
	fixture := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:0C34 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 987654 1 0000000000000000 100 0 0 10 0\n"
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	// No per-pid fd trees exist, so no owner can be found.

	original := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = original })

	got := Inspect(3124, 0)
	if !got.Listening {
		t.Fatalf("Inspect did not report the port as held: %+v", got)
	}
	if got.PID != 0 || got.Ours {
		t.Fatalf("Inspect attributed an unidentifiable owner: %+v", got)
	}
	if got.Inspectable {
		t.Fatalf("Inspect claimed the owner was inspectable: %+v", got)
	}
}

// TestAllPIDsFindsSelf sanity-checks the /proc scan the fallback relies on.
func TestAllPIDsFindsSelf(t *testing.T) {
	self := os.Getpid()
	for _, pid := range allPIDs() {
		if pid == self {
			return
		}
	}
	t.Skipf("cannot read %s; skipping pid scan check", procRoot)
}
