package process

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procRoot is overridable so tests can point the reader at a fixture.
var procRoot = "/proc"

// isZombie reports whether pid has already exited and is only waiting to be
// reaped. When the state cannot be read, the process is treated as alive, which
// is the conservative direction: it never invents a dead process.
func isZombie(pid int) bool {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	// The comm field is parenthesised and may itself contain spaces and
	// parentheses, so the state is the first field after the final ')'.
	idx := strings.LastIndexByte(string(data), ')')
	if idx < 0 || idx+2 >= len(data) {
		return false
	}
	return data[idx+2] == 'Z'
}
