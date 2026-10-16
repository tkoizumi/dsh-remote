package main

import (
	"os"
	"syscall"
	"testing"
)

// TestActionForTreatsHangupAsNoise is the regression guard for the silent-death
// outage: the proxy is started in the foreground of an SSH session, the session
// goes away, and the kernel delivers SIGHUP. Treating that as a stop request is
// what killed the proxy without running any teardown, left a stale state file,
// and stranded a DeepSeek Harness holding the port -- so the remote sessions
// dropped while the harness stayed up.
//
// Only explicit stop requests may end the run.
func TestActionForTreatsHangupAsNoise(t *testing.T) {
	cases := []struct {
		name string
		sig  os.Signal
		want signalAction
	}{
		{"hangup keeps serving", syscall.SIGHUP, signalContinue},
		{"interrupt stops", os.Interrupt, signalStop},
		{"terminate stops", syscall.SIGTERM, signalStop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := actionFor(tc.sig); got != tc.want {
				t.Fatalf("actionFor(%v) = %v, want %v", tc.sig, got, tc.want)
			}
		})
	}
}
