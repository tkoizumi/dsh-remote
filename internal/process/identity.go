package process

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// bootIDPath is overridable so tests can point the reader at a fixture.
var bootIDPath = "/proc/sys/kernel/random/boot_id"

// BootID returns the kernel's identifier for the current boot, or "" when it
// cannot be read.
//
// Recording it makes a state file left behind by a previous boot recognisable as
// stale. Without it, a pid recorded before a reboot can name a completely
// unrelated process afterwards, and "dsh-remote is already running" about a
// stranger wedges `start` until somebody deletes the state file by hand.
func BootID() string {
	data, err := os.ReadFile(bootIDPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// StartTime returns the start time of pid in clock ticks since boot -- field 22
// of /proc/<pid>/stat -- or 0 when it cannot be read.
//
// Together with the boot id this identifies a process for the whole life of the
// boot. Unlike a pid, a start time is never reused, so it is what makes a
// recorded pid verifiable rather than merely plausible.
func StartTime(pid int) int64 {
	if pid <= 0 {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	// comm is parenthesised and may itself contain spaces and parentheses, so
	// the fields are counted from the final ')' rather than by splitting the
	// whole line: nothing after comm contains a parenthesis.
	idx := strings.LastIndexByte(string(data), ')')
	if idx < 0 {
		return 0
	}
	fields := strings.Fields(string(data[idx+1:]))
	// After comm come state, ppid, pgrp, session, tty_nr, tpgid, flags, minflt,
	// cminflt, majflt, cmajflt, utime, stime, cutime, cstime, priority, nice,
	// num_threads, itrealvalue, starttime -- so starttime is index 19.
	if len(fields) < 20 {
		return 0
	}
	start, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil || start <= 0 {
		return 0
	}
	return start
}

// AliveSince reports whether pid still names the same live process that was
// recorded with startTime.
//
// A startTime of 0 means "not recorded" -- an older state file -- and degrades
// to a plain liveness check, so an existing deployment keeps working.
func AliveSince(pid int, startTime int64) bool {
	if !Alive(pid) {
		return false
	}
	if startTime <= 0 {
		return true
	}
	return StartTime(pid) == startTime
}

// ProxyAlive reports whether the process that wrote this run state is still that
// same process.
func (s *State) ProxyAlive() bool {
	if s == nil || s.PID <= 0 {
		return false
	}
	if !s.sameBoot() {
		return false
	}
	return AliveSince(s.PID, s.ProxyStart)
}

// DSHAlive reports whether the DeepSeek Harness child recorded in this run state
// is still that same process.
func (s *State) DSHAlive() bool {
	if s == nil || s.DSHPID <= 0 {
		return false
	}
	if !s.sameBoot() {
		return false
	}
	return AliveSince(s.DSHPID, s.DSHStart)
}

// sameBoot reports whether this state was written during the current boot. An
// unrecorded boot id is treated as matching, so state written by an older
// version is not wrongly discarded.
func (s *State) sameBoot() bool {
	if s.BootID == "" {
		return true
	}
	current := BootID()
	if current == "" {
		return true
	}
	return s.BootID == current
}
