//go:build linux

package socket

import (
	"os"
	"path/filepath"
	"testing"
)

// The tests in this file exercise the Linux-only /proc readers, which is why
// they carry the build tag: without it, `go test` on Darwin or a cross-platform
// vet fails on the missing helpers, which is exactly the mistake that broke the
// v0.1.9 release build.

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
