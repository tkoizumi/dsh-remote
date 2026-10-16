// Package socket attributes a loopback TCP port to the process listening on it.
//
// dsh-remote needs this to tell three situations apart after an unclean exit:
//
//   - the proxy is gone and nothing holds its port (a clean restart works),
//   - DeepSeek Harness that dsh-remote launched is still alive on port 3080 and
//     would make the next start fail on a port conflict (an orphan), and
//   - something unrelated holds the port, which dsh-remote must never touch.
//
// Linux is the only supported platform for the attribution; elsewhere the
// package reports the port as held but its owner as unknown, and callers refuse
// to touch it.
package socket

import "github.com/tkoizumi/dsh-remote/internal/process"

// Listener describes whoever holds a TCP port.
type Listener struct {
	// Port is the port that was probed.
	Port int
	// Listening is true when a socket in LISTEN state holds the port.
	Listening bool
	// PID is the owning process, or 0 when it could not be determined.
	PID int
	// PGID is the process group of the owning process, or 0 when it could not
	// be determined. dsh-remote launches DeepSeek Harness into a process group
	// of its own, so this is the durable identity behind the npx shim: it is
	// what lets callers recognise -- and reach -- the real node child.
	PGID int
	// Ours is true when the owner is the process group dsh-remote recorded as
	// the DeepSeek Harness tree it launched itself.
	Ours bool
	// Inspectable is false when the owner could not be read, so callers must
	// treat PID and Ours as unknown rather than as a negative answer.
	Inspectable bool
}

// Listening reports whether a socket in LISTEN state holds port.
//
// It is the cheap half of Inspect, for polling loops that only need to know
// whether a port has been released and do not need to attribute its owner.
func Listening(port int) bool {
	_, listening := inspectPort(port)
	return listening
}

// Inspect reports whether port is listening on the loopback interface and, when
// it is, which process owns it. recordedPID is the DeepSeek Harness pid from the
// dsh-remote run state; pass 0 when there is no recorded run.
func Inspect(port, recordedPID int) Listener {
	result := Listener{Port: port}
	inode, listening := inspectPort(port)
	if !listening {
		return result
	}
	result.Listening = true

	// Prefer the recorded child: when it is still alive it is the answer, and
	// it is attributed precisely without trusting a broad scan.
	if inode > 0 && recordedPID > 0 && process.Alive(recordedPID) {
		if ownsInode(recordedPID, inode) {
			result.PID = recordedPID
			result.PGID = process.ProcessGroup(recordedPID)
			result.Ours = true
			result.Inspectable = true
			return result
		}
	}
	if inode <= 0 {
		return result
	}
	pid, inspectable := inodeOwner(inode)
	result.PID = pid
	result.Inspectable = inspectable
	if pid > 0 {
		result.PGID = process.ProcessGroup(pid)
	}
	result.Ours = isOurs(pid, result.PGID, recordedPID)
	return result
}

// isOurs reports whether the process holding a port is the DeepSeek Harness tree
// that dsh-remote recorded in its run state.
//
// An exact pid match is not enough to recognise a real run. The recorded pid is
// the npx shim, while the process that binds the port is the node child the shim
// spawns, so those pids never match. What they do share is a process group:
// dsh-remote launches DeepSeek Harness with Setpgid (internal/dsh), which makes
// the recorded pid the group id as well, and a process group keeps its id after
// its leader exits. The child therefore stays attributable through the group
// precisely when it has become an orphan -- the case that must be recognised to
// be reclaimed. Reading the group this way, instead of reading the recorded
// process, is what makes the check survive the shim's exit.
func isOurs(pid, pgid, recordedPID int) bool {
	if pid <= 0 || recordedPID <= 0 {
		return false
	}
	if pid == recordedPID {
		return true
	}
	return pgid > 0 && pgid == recordedPID
}
