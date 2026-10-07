package dsh

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a mutex-guarded log sink: the child's output is relayed from
// its own goroutine while the test inspects the buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// writeScript creates a temporary executable shell script.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-dsh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartCapturesToken(t *testing.T) {
	script := writeScript(t, `echo "noise before"
echo "dsh web: http://127.0.0.1:3099/?token=TOKENVALUE"
sleep 30
`)
	var log syncBuffer
	proc, err := Start(context.Background(), Spec{Exec: script, Port: 3099}, &log)
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Stop(5 * time.Second)

	got, err := proc.WaitToken(10 * time.Second)
	if err != nil {
		t.Fatalf("WaitToken: %v", err)
	}
	if got.Token != "TOKENVALUE" {
		t.Fatalf("token = %q, want TOKENVALUE", got.Token)
	}
	if proc.PID() <= 0 {
		t.Fatalf("pid = %d, want > 0", proc.PID())
	}

	// The relayed log must not echo the credential back to the user.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(log.String(), "REDACTED") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(log.String(), "TOKENVALUE") {
		t.Fatalf("relayed log leaked the token:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "REDACTED") {
		t.Fatalf("relayed log did not redact the token line:\n%s", log.String())
	}
}

func TestWaitTokenReportsEarlyExit(t *testing.T) {
	script := writeScript(t, `echo "starting up"
exit 7
`)
	proc, err := Start(context.Background(), Spec{Exec: script, Port: 3099}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = proc.WaitToken(10 * time.Second)
	if err == nil {
		t.Fatal("expected an error when the child exits before printing a token")
	}
	if !strings.Contains(err.Error(), "exited with status 7") {
		t.Fatalf("error = %v, want it to mention exit status 7", err)
	}
}

func TestUnexpectedExitIsExposedOnDone(t *testing.T) {
	script := writeScript(t, `echo "dsh web: http://127.0.0.1:3099/?token=abc"
exit 3
`)
	proc, err := Start(context.Background(), Spec{Exec: script, Port: 3099}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proc.WaitToken(10 * time.Second); err != nil {
		t.Fatalf("WaitToken: %v", err)
	}
	select {
	case <-proc.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit")
	}
	if reason := ExitReason(proc.Err()); !strings.Contains(reason, "status 3") {
		t.Fatalf("ExitReason = %q, want it to mention status 3", reason)
	}
}

func TestWaitTokenTimesOut(t *testing.T) {
	script := writeScript(t, `sleep 30
`)
	proc, err := Start(context.Background(), Spec{Exec: script, Port: 3099}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Stop(5 * time.Second)
	if _, err := proc.WaitToken(150 * time.Millisecond); err == nil {
		t.Fatal("expected a timeout error")
	}
}

// TestStopKillsWholeProcessGroup verifies that a grandchild of DeepSeek
// Harness (the npx -> node tree in production) does not survive a stop.
func TestStopKillsWholeProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("CHILD_PID_FILE", pidFile)
	script := writeScript(t, `sleep 300 &
echo $! > "$CHILD_PID_FILE"
echo "dsh web: http://127.0.0.1:3099/?token=groupkill"
wait
`)
	proc, err := Start(context.Background(), Spec{Exec: script, Port: 3099}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proc.WaitToken(10 * time.Second); err != nil {
		t.Fatalf("WaitToken: %v", err)
	}

	var grandchild int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil && len(bytes.TrimSpace(data)) > 0 {
			grandchild = atoi(bytes.TrimSpace(data))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grandchild <= 0 {
		t.Fatal("fake dsh never reported its background child pid")
	}
	if !ProcessAlive(grandchild) {
		t.Fatal("background child should be alive before Stop")
	}

	if err := proc.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !ProcessAlive(grandchild) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("background child %d survived Stop; process group was not killed", grandchild)
}

func atoi(b []byte) int {
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
