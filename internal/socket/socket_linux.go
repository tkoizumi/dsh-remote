package socket

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// tcpStateListen is the state code in /proc/net/tcp for a listening socket.
const tcpStateListen = "0A"

// procRoot is a variable so tests can point the /proc readers at a fixture.
var procRoot = "/proc"

// inspectPort returns the socket inode of the LISTEN socket bound to port on
// any interface, or (0, false) when no such socket exists.
//
// IPv4 is read first because DeepSeek Harness and the proxy both bind
// 127.0.0.1; IPv6 is consulted too so an explicit `::1` bind is not missed.
func inspectPort(port int) (int, bool) {
	want := strings.ToUpper(strconv.FormatInt(int64(port), 16))
	if len(want) < 4 {
		want = strings.Repeat("0", 4-len(want)) + want
	}
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		if inode, ok := inodeFromNetTCP(filepath.Join(procRoot, name), want); ok {
			return inode, true
		}
	}
	return 0, false
}

// inodeFromNetTCP scans one /proc/net/tcp file for a LISTEN socket on wantPort
// (an uppercase, zero-padded hex port) and returns its inode.
func inodeFromNetTCP(path, wantPort string) (int, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// The header has fewer than four fields and names itself "local_address".
		if len(fields) < 10 || fields[3] != tcpStateListen {
			continue
		}
		_, port, ok := strings.Cut(fields[1], ":")
		if !ok || !strings.EqualFold(port, wantPort) {
			continue
		}
		inode, err := strconv.Atoi(fields[9])
		if err != nil || inode <= 0 {
			continue
		}
		return inode, true
	}
	return 0, false
}

// inodeOwner finds the live process holding the given socket inode.
//
// It first tries the recorded child (cheap and precise), then falls back to
// scanning the socket entries of every live process. Scanning reads
// /proc/<pid>/fd, which is only permitted for processes owned by this user, so
// a holder running as another user yields (0, false).
func inodeOwner(inode int) (int, bool) {
	pids := allPIDs()
	for _, pid := range pids {
		if ownsInode(pid, inode) {
			return pid, true
		}
	}
	return 0, false
}

// ownsInode reports whether pid has the socket inode open. An unreadable fd
// directory yields false: attribution is best-effort and never escalates.
func ownsInode(pid, inode int) bool {
	entries, err := os.ReadDir(filepath.Join(procRoot, strconv.Itoa(pid), "fd"))
	if err != nil {
		return false
	}
	want := "socket:[" + strconv.Itoa(inode) + "]"
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "fd", entry.Name()))
		if err != nil {
			continue
		}
		if target == want {
			return true
		}
	}
	return false
}

// allPIDs lists the numeric entries of /proc.
func allPIDs() []int {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}
