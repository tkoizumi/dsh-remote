package process

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// statFixture renders a /proc/<pid>/stat line with the fields this package
// reads. comm is passed through verbatim so callers can make it awkward.
func statFixture(pid int, comm, state string, pgrp int, start int64) string {
	// Fields after comm, in proc(5) order. Only state, pgrp and starttime carry
	// meaning for these tests, so everything else gets a harmless value. The
	// start time must land at index 19, which is field 22 on the line.
	after := []string{
		state,              // 3  state
		"1",                // 4  ppid
		strconv.Itoa(pgrp), // 5  pgrp
		"0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0",
		"20",                         // 19 nice
		"0",                          // 20 num_threads
		"1",                          // 21 itrealvalue
		strconv.FormatInt(start, 10), // 22 starttime
		"0", "0",
	}
	return fmt.Sprintf("%d (%s) %s\n", pid, comm, strings.Join(after, " "))
}

// TestStartTimeParsesField22 pins the parse against an awkward comm: the field
// is counted from the final ')' because comm may contain spaces and parentheses
// of its own.
func TestStartTimeParsesField22(t *testing.T) {
	root := t.TempDir()
	writeStat(t, root, 4242, statFixture(4242, "node (npx) worker", "S", 4100, 987654))
	pinProcRoot(t, root)

	if got := StartTime(4242); got != 987654 {
		t.Fatalf("StartTime = %d, want 987654", got)
	}
	if got := StartTime(9999); got != 0 {
		t.Fatalf("StartTime(missing pid) = %d, want 0", got)
	}
	if got := StartTime(0); got != 0 {
		t.Fatalf("StartTime(0) = %d, want 0", got)
	}
}

// TestBootIDReadsAndTrimsTheKernelValue also covers the failure direction: an
// unreadable boot id must yield "" so callers hide the check rather than invent
// a mismatch and declare live state stale.
func TestBootIDReadsAndTrimsTheKernelValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot_id")
	if err := os.WriteFile(path, []byte("1fd8457b-065c-4afd-bb36-03c1adb3d4bc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := bootIDPath
	t.Cleanup(func() { bootIDPath = original })

	bootIDPath = path
	if got := BootID(); got != "1fd8457b-065c-4afd-bb36-03c1adb3d4bc" {
		t.Fatalf("BootID = %q, want the trimmed kernel value", got)
	}

	bootIDPath = filepath.Join(t.TempDir(), "missing")
	if got := BootID(); got != "" {
		t.Fatalf("BootID(unreadable) = %q, want empty", got)
	}
}

// TestAliveSinceRejectsAReusedPid is the regression guard for the wedging bug a
// bare liveness check cannot see: the recorded pid is alive, but it belongs to a
// different process, so it must not be mistaken for the recorded run.
func TestAliveSinceRejectsAReusedPid(t *testing.T) {
	self := os.Getpid()
	start := StartTime(self)
	if start <= 0 {
		t.Skipf("cannot read %s; skipping", filepath.Join(procRoot, strconv.Itoa(self), "stat"))
	}

	if !AliveSince(self, start) {
		t.Fatal("AliveSince rejected the live process that was recorded")
	}
	if AliveSince(self, start+1) {
		t.Fatal("AliveSince accepted a mismatched start time; a reused pid would look like our own run")
	}
	if !AliveSince(self, 0) {
		t.Fatal("AliveSince(0) must degrade to a liveness check, so state from an older version still works")
	}
	if AliveSince(1<<30, 0) {
		t.Fatal("AliveSince accepted a pid that does not exist")
	}
}

// TestProxyAliveIsBootScoped proves the check that stops `start` from wedging
// forever after a reboot: state written during a previous boot must not block a
// new start even though its recorded pid happens to be alive now.
func TestProxyAliveIsBootScoped(t *testing.T) {
	self := os.Getpid()
	state := &State{PID: self, ProxyStart: StartTime(self)}
	if !state.ProxyAlive() {
		t.Fatal("ProxyAlive rejected state that names this very process")
	}

	path := filepath.Join(t.TempDir(), "boot_id")
	if err := os.WriteFile(path, []byte("current-boot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := bootIDPath
	t.Cleanup(func() { bootIDPath = original })
	bootIDPath = path

	state.BootID = "previous-boot"
	if state.ProxyAlive() {
		t.Fatal("ProxyAlive accepted state from a different boot; start would refuse forever")
	}
	state.BootID = "current-boot"
	if !state.ProxyAlive() {
		t.Fatal("ProxyAlive rejected state from the current boot")
	}
	state.BootID = ""
	if !state.ProxyAlive() {
		t.Fatal("ProxyAlive rejected legacy state with no boot id recorded")
	}

	// A wrong start time is stale regardless of the boot id.
	state.BootID = "current-boot"
	state.ProxyStart = state.ProxyStart + 1
	if state.ProxyAlive() {
		t.Fatal("ProxyAlive accepted a mismatched start time")
	}
}

// TestDSHAliveAppliesTheSameRules keeps the child's check in step with the
// proxy's: the recorded DeepSeek Harness pid must be the same process, not just
// a live one.
func TestDSHAliveAppliesTheSameRules(t *testing.T) {
	self := os.Getpid()
	state := &State{DSHPID: self, DSHStart: StartTime(self)}
	if !state.DSHAlive() {
		t.Fatal("DSHAlive rejected the live process that was recorded")
	}
	state.DSHStart = state.DSHStart + 1
	if state.DSHAlive() {
		t.Fatal("DSHAlive accepted a mismatched start time")
	}
	state.DSHStart = 0
	if !state.DSHAlive() {
		t.Fatal("DSHAlive(0 start) must degrade to a liveness check")
	}

	var nilState *State
	if nilState.ProxyAlive() || nilState.DSHAlive() {
		t.Fatal("a nil state must never report anything alive")
	}
}
