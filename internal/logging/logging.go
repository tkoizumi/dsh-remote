// Package logging writes a small, rotating, persistent log of dsh-remote's own
// decisions and of the DeepSeek Harness output it relays.
//
// It exists because the only record of an outage used to be whatever the
// terminal happened to show or whatever the journal captured. Both can be lost:
// a foreground run's output dies with the terminal, and journald only keeps
// logs across a reboot when /var/log/journal exists (Storage=auto). This file
// lives in the state directory instead, which by definition survives restarts
// and reboots, so a failure can still be explained afterwards.
//
// The token is never written. Every line passes through a redactor, and the
// caller additionally relays DeepSeek Harness output through the redacting
// writer exposed by the dsh package.
package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tkoizumi/dsh-remote/internal/process"
)

const (
	// MaxBytes is the size at which the log rotates.
	MaxBytes = 2 << 20 // 2 MiB
	// Backups is how many rotated generations are kept: 4 MiB of history.
	Backups = 2
	// FileName is the log's name inside the state directory.
	FileName = "dsh-remote.log"
)

// maxBytes is the rotation threshold. It is a variable so tests can drive
// rotation without writing megabytes.
var maxBytes = int64(MaxBytes)

// Logger appends timestamped lines to a file and, when the writer is provided,
// to an interactive sink as well. A write failure is reported once and then
// ignored: diagnostics must never take the proxy down.
type Logger struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	sink     io.Writer
	size     int64
	warned   bool
	disabled bool
	now      func() time.Time
}

// Path returns the log file location, honouring the same state-directory
// override as the run state so `status` and the running proxy always agree.
func Path() (string, error) {
	statePath, err := process.StatePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(statePath), FileName), nil
}

// Open starts a logger at path. A nil sink disables interactive echoing. The
// returned logger is always usable: when the file cannot be opened it still
// writes to the sink and reports the problem through it.
func Open(path string, sink io.Writer) *Logger {
	l := &Logger{path: path, sink: sink, now: time.Now}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		l.disabled = true
		l.warnf("cannot create log directory %s: %v", filepath.Dir(path), err)
		return l
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		l.disabled = true
		l.warnf("cannot open log file %s: %v", path, err)
		return l
	}
	info, err := file.Stat()
	if err == nil {
		l.size = info.Size()
	}
	l.file = file
	// Rotate immediately when a previous run left the file at the threshold, so
	// appending resumes within bounds.
	if l.size >= maxBytes {
		l.rotate()
	}
	return l
}

// Close flushes and closes the log file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Path reports the file this logger writes to.
func (l *Logger) Path() string { return l.path }

// Enabled reports whether entries are reaching the file.
func (l *Logger) Enabled() bool { return l.file != nil }

// Logf writes one timestamped entry. Callers use it for lifecycle events: what
// was decided, why, and with which pids.
func (l *Logger) Logf(format string, args ...any) {
	l.writeLine(fmt.Sprintf(format, args...))
}

// Write implements io.Writer for relayed DeepSeek Harness output. Each line is
// redacted again here, so a payload that bypassed the dsh package's redactor
// still cannot reach the file.
func (l *Logger) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		l.writeLine(RedactText(line))
	}
	return len(p), nil
}

// writeLine appends one entry to the file and echoes it with a timestamp.
//
// Redaction happens here rather than in Write, so a caller that formats a line
// with Logf is protected by the same backstop as relayed DeepSeek Harness
// output. The value-based redaction that actually knows the token still runs
// upstream, in dsh.Process.RedactingWriter.
func (l *Logger) writeLine(line string) {
	line = RedactText(line)

	l.mu.Lock()
	defer l.mu.Unlock()

	stamp := l.now().Format("2006-01-02T15:04:05.000Z07:00")
	entry := stamp + " " + line + "\n"

	if l.file != nil {
		// Rotate before the write when the entry would cross the threshold, so
		// the active file never exceeds it.
		if l.size+int64(len(entry)) > maxBytes {
			l.rotate()
		}
		if l.file != nil {
			n, err := l.file.WriteString(entry)
			if err != nil {
				l.warnf("log write failed: %v", err)
			} else {
				l.size += int64(n)
			}
		}
	}
	if l.sink != nil {
		fmt.Fprintln(l.sink, line)
	}
}

// rotate shifts the generations and reopens an empty active file. Callers must
// hold l.mu.
func (l *Logger) rotate() {
	if l.file == nil {
		return
	}
	_ = l.file.Close()
	l.file = nil

	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, Backups))
	for i := Backups - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
	}
	_ = os.Rename(l.path, l.path+".1")

	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		l.disabled = true
		l.warnf("cannot reopen log file %s after rotation: %v", l.path, err)
		return
	}
	l.file = file
	l.size = 0
}

func (l *Logger) warnf(format string, args ...any) {
	if l.warned || l.sink == nil {
		return
	}
	l.warned = true
	fmt.Fprintf(l.sink, "warning: "+format+"\n", args...)
}

// Status summarises the log file for `status` and `doctor`.
type Status struct {
	Path string
	// Present is true when a log file exists.
	Present bool
	// Size is the active file's size in bytes.
	Size int64
	// Modified is the active file's last write time, zero when absent.
	Modified time.Time
	// LastLine is the most recent entry, trimmed, for a quick look at how the
	// last run ended.
	LastLine string
	// Err records why the log could not be inspected.
	Err error
}

// Inspect reads the log's metadata and its final line. It never returns more
// than one line of content, so a diagnostic cannot dump the whole history.
func Inspect(path string) Status {
	status := Status{Path: path}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return status
	}
	if err != nil {
		status.Err = err
		return status
	}
	status.Present = true
	status.Size = info.Size()
	status.Modified = info.ModTime()
	status.LastLine = lastLine(path)
	return status
}

// lastLine returns the final non-empty line of a file, reading at most the last
// 64 KiB so a large log is never loaded whole.
func lastLine(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return ""
	}
	const window = 64 << 10
	start := info.Size() - window
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(file, window))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return strings.TrimSpace(lines[i])
		}
	}
	return ""
}
