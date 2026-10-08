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
	// Ours is true when PID matches the process dsh-remote recorded as the
	// DeepSeek Harness child it launched itself.
	Ours bool
	// Inspectable is false when the owner could not be read, so callers must
	// treat PID and Ours as unknown rather than as a negative answer.
	Inspectable bool
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
	result.Ours = pid > 0 && pid == recordedPID
	return result
}
