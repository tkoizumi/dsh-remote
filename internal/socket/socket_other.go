//go:build !linux

package socket

import (
	"net"
	"strconv"
	"time"
)

// inspectPort is the non-Linux fallback. It can tell that the port is held, but
// it cannot say by which process, so it reports a zero inode. Callers then see
// an uninspectable listener and refuse to touch it, which is the safe direction.
func inspectPort(port int) (int, bool) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return 0, false
	}
	_ = conn.Close()
	return 0, true
}
