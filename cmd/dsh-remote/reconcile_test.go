package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/process"
	"github.com/tkoizumi/dsh-remote/internal/socket"
	"github.com/tkoizumi/dsh-remote/internal/systemd"
)

// orphanHelperEnv makes the test binary behave as a stand-in for the DeepSeek
// Harness child: it binds a port and then blocks, so the test can reproduce the
// state left behind when the proxy is killed without running its cleanup.
const orphanHelperEnv = "DSH_REMOTE_TEST_ORPHAN_PORT"

// orphanHelperPidEnv is written by the helper so the parent knows the pid of the
// process that actually holds the port (the parent of the pipe).
const orphanHelperPidEnv = "DSH_REMOTE_TEST_ORPHAN_PIDFILE"

// TestMain doubles as the helper entry point. A child started with
// orphanHelperEnv set never runs the test suite.
func TestMain(m *testing.M) {
	if port := os.Getenv(orphanHelperEnv); port != "" {
		runOrphanHelper(port)
		return
	}
	os.Exit(m.Run())
}

// runOrphanHelper binds the requested port in its own process group and blocks
// until it is signalled, mimicking a DeepSeek Harness child whose parent died.
func runOrphanHelper(port string) {
	_ = syscall.Setpgid(0, 0)
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: listen: %v\n", err)
		os.Exit(3)
	}
	// The test reads the real holder's pid from this file. It differs from the
	// pid the test spawned, because go test runs the binary through a shim.
	if path := os.Getenv(orphanHelperPidEnv); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	fmt.Println("listening")
	// Hold the listener open until the test kills this process.
	for {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			os.Exit(4)
		}
		_ = conn.Close()
	}
}

// startOrphan launches the helper and waits until it holds the port, returning
// the pid that owns the socket.
func startOrphan(t *testing.T) (pid, port int) {
	t.Helper()

	// Reserve a port, then release it for the helper to take.
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = reserved.Addr().(*net.TCPAddr).Port
	_ = reserved.Close()

	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append(os.Environ(),
		orphanHelperEnv+"="+strconv.Itoa(port),
		orphanHelperPidEnv+"="+pidFile,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Kill only this helper's own process group. Signalling the test's
		// group (or pid 0) would kill the test binary itself.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	ready := bufio.NewScanner(stdout)
	if !ready.Scan() || ready.Text() != "listening" {
		t.Fatalf("orphan helper did not start listening: %q", ready.Text())
	}

	// Wait for the pid file so the returned pid is the process that really
	// holds the socket, not the shim in front of it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			holder, convErr := strconv.Atoi(string(data))
			if convErr != nil {
				t.Fatalf("helper pid file is malformed: %q", data)
			}
			return holder, port
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("orphan helper never reported its pid")
	return 0, 0
}

// TestPreflightReclaimsOrphanedDSH is the regression test for the incident: the
// proxy is gone, the DeepSeek Harness it launched still holds its port, and the
// preflight must clear that process so a supervised restart can succeed instead
// of failing forever on the port conflict.
func TestPreflightReclaimsOrphanedDSH(t *testing.T) {
	pid, port := startOrphan(t)

	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("DSH_REMOTE_STATE_FILE", statePath)
	if err := process.SaveState(&process.State{PID: os.Getpid(), DSHPID: pid}); err != nil {
		t.Fatal(err)
	}

	if got := socket.Inspect(port, pid); !got.Listening || !got.Ours {
		t.Fatalf("test setup: orphan not attributed to the recorded pid: %+v", got)
	}

	action, err := dshPortPreflight(port, &process.State{PID: os.Getpid(), DSHPID: pid}, nil)
	if err != nil {
		t.Fatalf("preflight refused to reclaim our own orphan: %v", err)
	}
	if action != portReclaimed {
		t.Fatalf("preflight action = %v, want portReclaimed", action)
	}
	if process.Alive(pid) {
		t.Fatalf("orphan pid %d survived the preflight", pid)
	}
	if got := socket.Inspect(port, pid); got.Listening {
		t.Fatalf("port %d is still held after the preflight", port)
	}
	if state, err := process.LoadState(); err != nil || state != nil {
		t.Fatalf("stale run state was not cleared: state=%+v err=%v", state, err)
	}
}

// TestPreflightRefusesForeignProcess is the safety guard: a DeepSeek Harness
// that dsh-remote did not launch must never be stopped. Killing it would destroy
// a session the user started by hand.
func TestPreflightRefusesForeignProcess(t *testing.T) {
	_, port := startOrphan(t)

	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("DSH_REMOTE_STATE_FILE", statePath)

	// A recorded pid that did not launch this listener, or no record at all.
	action, err := dshPortPreflight(port, nil, nil)
	if err == nil {
		t.Fatal("preflight accepted a foreign port holder")
	}
	if action != portForeign {
		t.Fatalf("preflight action = %v, want portForeign", action)
	}
	if got := socket.Inspect(port, 0); !got.Listening {
		t.Fatal("preflight stopped a process it did not launch")
	}
}

// TestPreflightFreePort proves the ordinary start path is untouched.
func TestPreflightFreePort(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reserved.Addr().(*net.TCPAddr).Port
	_ = reserved.Close()

	action, err := dshPortPreflight(port, nil, nil)
	if err != nil {
		t.Fatalf("preflight on a free port: %v", err)
	}
	if action != portFree {
		t.Fatalf("preflight action = %v, want portFree", action)
	}
}

// doctorWith builds a doctor pass with supervision stubbed, so the checks can be
// exercised without a systemd bus.
func doctorWith(d diagnose, supervision systemd.State) diagnose {
	d.Supervision = supervision
	return d
}
