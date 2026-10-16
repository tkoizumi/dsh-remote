package process

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ProcessGroup returns the process group id of pid, or 0 when it cannot be read.
//
// dsh-remote launches DeepSeek Harness with Setpgid (internal/dsh), so the pid
// it records is also the id of a process group that holds the whole npx -> node
// tree. That group is the durable identity: a process group keeps its id after
// its leader exits, so the real node child stays attributable -- and reachable --
// when the shim that led the group is already gone.
func ProcessGroup(pid int) int {
	if pid <= 0 {
		return 0
	}
	state, pgrp, ok := statGroup(pid)
	if !ok || state == 'Z' {
		// A zombie has already exited and released its sockets; it is not a
		// live member of anything.
		return 0
	}
	return pgrp
}

// GroupAlive reports whether any live process still belongs to the process group
// led by pgid.
//
// Zombies deliberately do not count. A killed process stays in the kernel's task
// list until its parent reaps it, but it has already exited and closed its
// sockets, so treating it as a live member would report a successfully reclaimed
// port as a failure while the reaper -- init, or a test -- has not run yet.
//
// An unreadable /proc yields false, which callers read as "nothing left to
// stop". That is the safe direction here because the caller verifies the port is
// actually free afterwards and reports an error when it is not.
func GroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		pid, convErr := strconv.Atoi(entry.Name())
		if convErr != nil || pid <= 0 {
			continue
		}
		state, pgrp, ok := statGroup(pid)
		if !ok || state == 'Z' {
			continue
		}
		if pgrp == pgid {
			return true
		}
	}
	return false
}

// statGroup reads the state and process group of one process from /proc.
//
// The format is "pid (comm) state ppid pgrp ...", and comm may itself contain
// spaces and parentheses, so the fields are counted from the final ')' rather
// than by splitting the whole line: nothing after comm contains a parenthesis.
func statGroup(pid int) (state byte, pgrp int, ok bool) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, false
	}
	idx := strings.LastIndexByte(string(data), ')')
	if idx < 0 || idx+2 >= len(data) {
		return 0, 0, false
	}
	state = data[idx+2]

	fields := strings.Fields(string(data[idx+1:]))
	// After comm come: state, ppid, pgrp.
	if len(fields) < 3 {
		return 0, 0, false
	}
	pgrp, err = strconv.Atoi(fields[2])
	if err != nil || pgrp <= 0 {
		return 0, 0, false
	}
	return state, pgrp, true
}
