//go:build !linux

package socket

import "testing"

// TestInspectOffLinuxIsUnattributable is the compile-and-behaviour guard for the
// non-Linux build.
//
// The incident-era release failed because the shared code called helpers that
// only existed in the Linux file, and every Linux target still compiled and
// passed. This test exists so `go test` on a non-Linux platform -- and a
// cross-platform build in CI -- actually exercises this half of the package:
// Inspect must report the port as held but its owner as unknown, which is what
// makes the caller refuse to reclaim it instead of guessing.
func TestInspectOffLinuxIsUnattributable(t *testing.T) {
	got := Inspect(3124, 0)
	if got.Listening {
		t.Fatalf("Inspect reported a listener on a port that is not held: %+v", got)
	}
	if got.PID != 0 || got.Ours || got.Inspectable {
		t.Fatalf("Inspect attributed an owner off Linux: %+v", got)
	}

	// The helpers themselves must be present and answer negatively.
	if ownsInode(1, 1) {
		t.Fatal("ownsInode claimed ownership off Linux")
	}
	if pid, inspectable := inodeOwner(1); pid != 0 || inspectable {
		t.Fatalf("inodeOwner = (%d, %t), want (0, false)", pid, inspectable)
	}
}
