package process

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeStat plants one /proc/<pid>/stat fixture under root, so the readers in
// this package can be exercised without depending on the host's processes.
func writeStat(t *testing.T, root string, pid int, stat string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
		t.Fatal(err)
	}
}

// pinProcRoot points the /proc readers at a fixture for the duration of a test.
func pinProcRoot(t *testing.T, root string) {
	t.Helper()
	original := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = original })
}

// TestProcessGroupParsesNestedComm pins the parsing rule for the field that
// carries the group id. comm is parenthesised and may itself contain spaces and
// parentheses -- "node (npx)" here -- so the group has to be read relative to
// the final ')' rather than by splitting the whole line.
func TestProcessGroupParsesNestedComm(t *testing.T) {
	root := t.TempDir()
	writeStat(t, root, 4242, "4242 (node (npx)) S 4100 4100 4100 0 -1\n")
	pinProcRoot(t, root)

	if got := ProcessGroup(4242); got != 4100 {
		t.Fatalf("ProcessGroup = %d, want 4100", got)
	}
	if got := ProcessGroup(0); got != 0 {
		t.Fatalf("ProcessGroup(0) = %d, want 0", got)
	}
}

// TestProcessGroupRejectsZombie keeps an exited-but-unreaped process from
// counting as a group: it has already released the socket the caller cares
// about, so it must not keep a reclaimed port looking occupied.
func TestProcessGroupRejectsZombie(t *testing.T) {
	root := t.TempDir()
	writeStat(t, root, 4243, "4243 (dsh) Z 1 4100 4100 0 -1\n")
	pinProcRoot(t, root)

	if got := ProcessGroup(4243); got != 0 {
		t.Fatalf("ProcessGroup(zombie) = %d, want 0", got)
	}
}

// TestGroupAliveIgnoresZombies is the property that lets the reclaim path finish
// promptly. A killed process stays in the kernel's task list until its parent
// reaps it, so a group whose members have all exited still answers kill(-pgid, 0)
// -- but it holds nothing, and waiting on it would report a successful reclaim as
// a failure.
func TestGroupAliveIgnoresZombies(t *testing.T) {
	root := t.TempDir()
	// The shim (5101) led the group; the node child (5102) inherited it. Both
	// have been killed and are waiting to be reaped.
	writeStat(t, root, 5101, "5101 (npx) Z 1 5100 5100 0 -1\n")
	writeStat(t, root, 5102, "5102 (node) Z 5101 5100 5100 0 -1\n")
	pinProcRoot(t, root)

	if GroupAlive(5100) {
		t.Fatal("GroupAlive counted zombies as live members")
	}

	// A live member makes the group live again.
	writeStat(t, root, 5102, "5102 (node) S 5101 5100 5100 0 -1\n")
	if !GroupAlive(5100) {
		t.Fatal("GroupAlive missed a live member of the group")
	}

	// A live process in another group must never be counted.
	if GroupAlive(9999) {
		t.Fatal("GroupAlive reported an unrelated group as alive")
	}
	if GroupAlive(0) {
		t.Fatal("GroupAlive(0) reported a group as alive")
	}
}
