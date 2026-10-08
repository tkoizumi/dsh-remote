package main

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/socket"
)

// orphanStopTimeout is how long an orphaned DeepSeek Harness gets to exit
// cleanly before it is killed outright.
const orphanStopTimeout = 10 * time.Second

// portPreflight decides what to do about the DeepSeek Harness port before
// launching a new child.
//
// This is the fix for the outage mode where the proxy dies without running its
// cleanup: the child was placed in its own process group, so it survives its
// parent, keeps port 3080, and makes every supervised restart fail with a port
// conflict. `Restart=on-failure` then retries forever against a port that will
// never free itself.
type portPreflight int

const (
	// portFree: nothing holds the port; start normally.
	portFree portPreflight = iota
	// portReclaimed: the orphan was cleared and starting may proceed.
	portReclaimed
	// portForeign: a process dsh-remote did not launch holds the port.
	portForeign
	// portUnknown: the holder could not be identified, so it is left alone.
	portUnknown
)

// dshPortPreflight inspects the DSH port and, when it is held by a DeepSeek
// Harness that a previous dsh-remote run launched, stops that leftover process
// and its process group so the port is free for the new child.
//
// It never touches a process that cannot be attributed to dsh-remote: a
// manually started `dsh web` is a legitimate setup and killing it would destroy
// someone's running session.
func dshPortPreflight(dshPort int, recorded *process.State, logf func(string, ...any)) (portPreflight, error) {
	recordedPID := recordedDSHPID(recorded)
	owner := socket.Inspect(dshPort, recordedPID)
	if !owner.Listening {
		return portFree, nil
	}
	if !owner.Ours {
		if !owner.Inspectable && owner.PID == 0 {
			return portUnknown, fmt.Errorf(
				"port %d is in use but its owner could not be identified; refusing to touch a process dsh-remote may not have started", dshPort)
		}
		return portForeign, fmt.Errorf(
			"port %d is in use by pid %d, which dsh-remote did not launch; stop it yourself, or run `dsh-remote start --dsh-port <other port>`", dshPort, owner.PID)
	}

	// Attributed to the child recorded in run state, and that child's proxy is
	// gone: this is the orphan. Ask the whole process group to stop, because
	// npx is only the parent of the real node process.
	if logf != nil {
		logf("stopping leftover DeepSeek Harness (pid %d) still holding port %d from an earlier run", owner.PID, dshPort)
	}
	if err := stopProcessGroup(owner.PID, orphanStopTimeout); err != nil {
		return portUnknown, fmt.Errorf("stop leftover DeepSeek Harness (pid %d): %w", owner.PID, err)
	}
	if still := socket.Inspect(dshPort, recordedPID); still.Listening {
		return portUnknown, fmt.Errorf("port %d is still held after stopping pid %d", dshPort, owner.PID)
	}
	if recorded != nil {
		if err := process.RemoveState(); err != nil {
			return portReclaimed, fmt.Errorf("removed the leftover process but could not clear stale run state: %w", err)
		}
	}
	return portReclaimed, nil
}

// stopProcessGroup terminates pid and everything in its process group, which is
// how the npx -> node tree is actually reached. It escalates to SIGKILL after
// timeout and reports an error only when the process is still alive at the end.
func stopProcessGroup(pid int, timeout time.Duration) error {
	if pid <= 0 || !process.Alive(pid) {
		return nil
	}
	_ = signalGroup(pid, syscall.SIGTERM)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !process.Alive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = signalGroup(pid, syscall.SIGKILL)
	for i := 0; i < 50; i++ {
		if !process.Alive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("process is still alive after SIGKILL")
}

// signalGroup signals the process group led by pid, falling back to the single
// process when the group has already been dissolved.
func signalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

// describePreflight renders a preflight outcome for a human.
func describePreflight(action portPreflight) string {
	switch action {
	case portFree:
		return "the DeepSeek Harness port is free"
	case portReclaimed:
		return "reclaimed the DeepSeek Harness port from a leftover process"
	case portForeign:
		return "the DeepSeek Harness port is held by a process dsh-remote did not launch"
	case portUnknown:
		return "the DeepSeek Harness port holder could not be identified"
	}
	return "unknown preflight result"
}
