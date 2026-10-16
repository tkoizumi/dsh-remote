package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// shimHelperEnv makes the test binary behave as a stand-in for the npx shim: it
// leads a process group of its own, spawns the port holder as a child inside
// that group, and stays alive. This is the topology dsh-remote actually creates,
// and the reason the pid it records is never the pid that binds the port.
const shimHelperEnv = "DSH_REMOTE_TEST_SHIM_PORT"

// shimHelperPidEnv is where the shim reports its own pid, which is the pid
// dsh-remote would have recorded in run state.
const shimHelperPidEnv = "DSH_REMOTE_TEST_SHIM_PIDFILE"

// orphanInheritGroupEnv keeps the listener in the shim's process group instead
// of creating a group of its own, which is what npx does with the node child.
const orphanInheritGroupEnv = "DSH_REMOTE_TEST_ORPHAN_INHERIT_GROUP"

// TestMain doubles as the helper entry point. A child started with
// orphanHelperEnv or shimHelperEnv set never runs the test suite.
func TestMain(m *testing.M) {
	if port := os.Getenv(orphanHelperEnv); port != "" {
		runOrphanHelper(port)
		return
	}
	if port := os.Getenv(shimHelperEnv); port != "" {
		runShimHelper(port)
		return
	}
	os.Exit(m.Run())
}

// runOrphanHelper binds the requested port and blocks until it is signalled,
// mimicking a DeepSeek Harness child whose parent died. It leads its own process
// group unless it was spawned by the shim helper, where it must inherit the
// shim's group to reproduce the npx -> node tree.
func runOrphanHelper(port string) {
	if os.Getenv(orphanInheritGroupEnv) == "" {
		_ = syscall.Setpgid(0, 0)
	}
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

// runShimHelper leads a process group, spawns the port holder inside it, and then
// blocks. The two pids stay distinct by construction, which is what the
// pid-only attribution could not cope with.
func runShimHelper(port string) {
	_ = syscall.Setpgid(0, 0)
	if path := os.Getenv(shimHelperPidEnv); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	child := exec.Command(os.Args[0], "-test.run=TestMain")
	child.Env = append(os.Environ(),
		orphanHelperEnv+"="+port,
		orphanInheritGroupEnv+"=1",
	)
	// No Setpgid here: the child must stay in this process's group, the way the
	// node process stays in the group npx was launched into.
	child.Stdout = io.Discard
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "shim: start listener: %v\n", err)
		os.Exit(5)
	}
	// Stay alive as the group leader until the group is signalled. A registered
	// timer is deliberate: select{} on its own risks the runtime's deadlock
	// detector, which would exit the shim before the test can use it.
	for {
		time.Sleep(time.Hour)
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

// startShimmedOrphan launches the npx-shaped topology: a shim that leads its own
// process group and a listener spawned inside that group. It returns the pid
// dsh-remote would have recorded (the shim) and the pid that really holds the
// socket (the shim's child), which are never the same process.
func startShimmedOrphan(t *testing.T) (shimPID, holderPID, port int) {
	t.Helper()

	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = reserved.Addr().(*net.TCPAddr).Port
	_ = reserved.Close()

	dir := t.TempDir()
	shimPidFile := filepath.Join(dir, "shim.pid")
	holderPidFile := filepath.Join(dir, "holder.pid")

	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append(os.Environ(),
		shimHelperEnv+"="+strconv.Itoa(port),
		shimHelperPidEnv+"="+shimPidFile,
		orphanHelperPidEnv+"="+holderPidFile,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	shimPID = waitForPIDFile(t, shimPidFile)
	holderPID = waitForPIDFile(t, holderPidFile)

	t.Cleanup(func() {
		// Kill the shim's own group (which contains the listener) and then the
		// process the test exec'd, never the test's own group.
		_ = syscall.Kill(-shimPID, syscall.SIGKILL)
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	if holderPID == shimPID {
		t.Fatalf("test setup: shim pid %d also holds the port; the topology is not a shim", shimPID)
	}

	// The holder must be attributable through the shim's group, because its pid
	// deliberately does not match what was recorded.
	got := socket.Inspect(port, shimPID)
	if !got.Listening {
		t.Fatalf("test setup: port %d is not listening yet", port)
	}
	if got.PID != holderPID {
		t.Fatalf("test setup: port %d held by pid %d, want the shim child %d", port, got.PID, holderPID)
	}
	if !got.Ours {
		t.Fatalf("test setup: holder %d was not attributed to recorded shim %d: %+v", holderPID, shimPID, got)
	}
	if got.PGID != shimPID {
		t.Fatalf("test setup: holder %d is in group %d, want the shim's group %d", holderPID, got.PGID, shimPID)
	}
	return shimPID, holderPID, port
}

// waitForPIDFile waits for a helper to report its own pid.
func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr != nil || pid <= 0 {
				t.Fatalf("pid file %s is malformed: %q", path, data)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper never reported its pid in %s", path)
	return 0
}

// waitUntil polls a condition, for states that settle asynchronously.
func waitUntil(t *testing.T, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before the deadline")
}

// TestPreflightReclaimsOrphanBehindLiveShim is the regression test for the
// production topology: dsh-remote recorded the npx shim's pid, the port is held
// by the node child the shim spawned, and the proxy that would have cleaned up
// after itself is gone. Pid-only attribution called that child a foreign process
// and refused, so every supervised restart failed on the same port conflict and
// the outage outlived the process that caused it.
func TestPreflightReclaimsOrphanBehindLiveShim(t *testing.T) {
	shimPID, holderPID, port := startShimmedOrphan(t)

	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("DSH_REMOTE_STATE_FILE", statePath)
	recorded := &process.State{PID: os.Getpid(), DSHPID: shimPID}
	if err := process.SaveState(recorded); err != nil {
		t.Fatal(err)
	}

	action, err := dshPortPreflight(port, recorded, nil)
	if err != nil {
		t.Fatalf("preflight refused to reclaim its own orphan behind a shim: %v", err)
	}
	if action != portReclaimed {
		t.Fatalf("preflight action = %v, want portReclaimed", action)
	}
	if got := socket.Inspect(port, shimPID); got.Listening {
		t.Fatalf("port %d is still held after the preflight", port)
	}
	if process.Alive(holderPID) {
		t.Fatalf("port holder %d survived the preflight", holderPID)
	}
	if state, err := process.LoadState(); err != nil || state != nil {
		t.Fatalf("stale run state was not cleared: state=%+v err=%v", state, err)
	}
}

// TestPreflightReclaimsOrphanWhoseShimExited covers the case that actually
// happens in the wild: by the time the port is inspected, the shim is already
// gone and the recorded pid is dead. Only the process group still identifies the
// child -- and because the group id outlives its leader, the orphan stays
// attributable. Liveness has to be judged by the group too: checking the
// recorded pid alone reported success without stopping anything, leaving the
// port held and the restart loop running.
func TestPreflightReclaimsOrphanWhoseShimExited(t *testing.T) {
	shimPID, holderPID, port := startShimmedOrphan(t)

	// Kill the shim, leaving its child holding the port in the shim's group.
	if err := syscall.Kill(shimPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill shim %d: %v", shimPID, err)
	}
	waitUntil(t, 5*time.Second, func() bool { return !process.Alive(shimPID) })

	got := socket.Inspect(port, shimPID)
	if !got.Listening || !got.Ours {
		t.Fatalf("orphan behind a dead shim was not attributed: %+v", got)
	}
	if got.PID != holderPID || got.PGID != shimPID {
		t.Fatalf("attribution = pid %d group %d, want pid %d group %d", got.PID, got.PGID, holderPID, shimPID)
	}

	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("DSH_REMOTE_STATE_FILE", statePath)
	recorded := &process.State{PID: os.Getpid(), DSHPID: shimPID}
	if err := process.SaveState(recorded); err != nil {
		t.Fatal(err)
	}

	action, err := dshPortPreflight(port, recorded, nil)
	if err != nil {
		t.Fatalf("preflight refused to reclaim an orphan whose shim had exited: %v", err)
	}
	if action != portReclaimed {
		t.Fatalf("preflight action = %v, want portReclaimed", action)
	}
	if process.Alive(holderPID) {
		t.Fatalf("port holder %d survived the preflight", holderPID)
	}
	if got := socket.Inspect(port, shimPID); got.Listening {
		t.Fatalf("port %d is still held after the preflight", port)
	}
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
