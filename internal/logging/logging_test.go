package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoggerWritesTimestampedLinesAndSurvivesReopen is the core persistence
// property: entries are on disk, and a later process reading the same file still
// sees them. That is what makes a failure explainable after a restart.
func TestLoggerWritesTimestampedLinesAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", FileName)

	log := Open(path, nil)
	if !log.Enabled() {
		t.Fatal("logger did not open a writable file")
	}
	log.Logf("dsh-remote %s starting", "0.1.0")
	log.Logf("proxy: listening on 127.0.0.1:3081")
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	// A second Open stands in for the next run or a later diagnostic.
	second := Open(path, nil)
	second.Logf("shutdown: received terminated")
	_ = second.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"dsh-remote 0.1.0 starting", "proxy: listening on 127.0.0.1:3081", "shutdown: received terminated"} {
		if !strings.Contains(text, want) {
			t.Errorf("log is missing %q\n---\n%s", want, text)
		}
	}
	// Every line must begin with a timestamp so an outage window is readable.
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if len(line) < 20 || line[4] != '-' || line[10] != 'T' {
			t.Errorf("line is not timestamped: %q", line)
		}
	}
	if mode := fileMode(t, path); mode != 0o600 {
		t.Errorf("log mode = %o, want 600", mode)
	}
}

// TestLoggerNeverWritesAToken is the security regression test: whatever the
// caller relays, a token-shaped query parameter and a cookie header must not
// reach the file, and a known token must be removed by the caller's redactor.
func TestLoggerNeverWritesAToken(t *testing.T) {
	const token = "ykUIFiyl2GcfrTIaUAth_iT7l_yZZkKhjvEABM6WxAo"
	const cookie = "dsh_auth=abcdef0123456789"

	path := filepath.Join(t.TempDir(), FileName)
	log := Open(path, nil)

	// What DeepSeek Harness actually prints at startup.
	log.Write([]byte("dsh web: http://127.0.0.1:3080/?token=" + token + "\n"))
	// A line that carries a credential without a token= parameter.
	log.Write([]byte("Set-Cookie: " + cookie + "; Path=/; HttpOnly\n"))
	// A line that names the credential and then prints its value.
	log.Logf("dsh: startup token: %s", token)
	_ = log.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, token) {
		t.Fatalf("log contains the token:\n%s", text)
	}
	if strings.Contains(text, cookie) {
		t.Fatalf("log contains a cookie value:\n%s", text)
	}
	if !strings.Contains(text, "token="+RedactedMarker) {
		t.Errorf("token parameter was not marked as redacted:\n%s", text)
	}
	if !strings.Contains(text, "Set-Cookie: "+RedactedMarker) {
		t.Errorf("cookie header was not marked as redacted:\n%s", text)
	}
}

// TestLoggerRotatesAndKeepsGenerations pins bounded disk use: a log that never
// rotates would eventually fill the state directory, but the history must still
// survive more than one generation.
func TestLoggerRotatesAndKeepsGenerations(t *testing.T) {
	// Drive rotation with a small threshold rather than megabytes of fixtures.
	original := maxBytes
	maxBytes = 2048
	t.Cleanup(func() { maxBytes = original })

	dir := t.TempDir()
	path := filepath.Join(dir, FileName)

	log := Open(path, nil)
	line := strings.Repeat("x", 512)
	for i := 0; i < 40; i++ {
		log.Logf("entry %d %s", i, line)
	}
	_ = log.Close()

	active, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if active.Size() > maxBytes {
		t.Errorf("active log grew past the threshold: %d > %d", active.Size(), maxBytes)
	}
	for i := 1; i <= Backups; i++ {
		backup := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(backup); err != nil {
			t.Errorf("expected rotated generation %s: %v", backup, err)
		}
	}
	// The oldest generation must be discarded, so the total stays bounded.
	if _, err := os.Stat(fmt.Sprintf("%s.%d", path, Backups+1)); err == nil {
		t.Errorf("kept more than %d generations", Backups)
	}
}

// TestInspectReportsLastLine proves a diagnostic can show how the last run ended
// without dumping the whole file.
func TestInspectReportsLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	log := Open(path, nil)
	log.Logf("first")
	log.Logf("shutdown: DeepSeek Harness exited with status 1")
	_ = log.Close()

	status := Inspect(path)
	if !status.Present {
		t.Fatal("Inspect did not see the log")
	}
	if status.Size <= 0 {
		t.Errorf("Size = %d, want > 0", status.Size)
	}
	if status.Modified.IsZero() {
		t.Error("Modified is zero")
	}
	if !strings.Contains(status.LastLine, "shutdown: DeepSeek Harness exited with status 1") {
		t.Errorf("LastLine = %q", status.LastLine)
	}
	if strings.Count(status.LastLine, "\n") != 0 {
		t.Errorf("LastLine should be a single line: %q", status.LastLine)
	}
}

// TestInspectMissingFileIsNotAnError keeps the first-run case quiet.
func TestInspectMissingFileIsNotAnError(t *testing.T) {
	status := Inspect(filepath.Join(t.TempDir(), FileName))
	if status.Present || status.Err != nil {
		t.Fatalf("Inspect(missing) = %+v, want absent and no error", status)
	}
}

// TestOpenUnwritablePathStillEchoes proves logging never takes the proxy down:
// when the file cannot be created, the failure is reported to the sink and the
// process continues.
func TestOpenUnwritablePathStillEchoes(t *testing.T) {
	// A path whose parent is a regular file cannot be created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var sink strings.Builder
	log := Open(filepath.Join(blocker, "dsh-remote.log"), &sink)
	if log.Enabled() {
		t.Fatal("logger reported enabled for an impossible path")
	}
	log.Logf("this should still reach the operator")
	if !strings.Contains(sink.String(), "cannot") {
		t.Errorf("no warning was emitted to the sink: %q", sink.String())
	}
}

// TestEchoKeepsSinkUnchangedAndUnstamped checks the interactive copy stays
// readable: the timestamp belongs in the file, not line-by-line on the terminal.
func TestEchoKeepsSinkUnchangedAndUnstamped(t *testing.T) {
	var sink strings.Builder
	log := Open(filepath.Join(t.TempDir(), FileName), &sink)
	log.Logf("hello operator")
	_ = log.Close()

	if got := strings.TrimSpace(sink.String()); got != "hello operator" {
		t.Fatalf("sink got %q, want %q", got, "hello operator")
	}
}

// TestRedactTextHandlesQueryAndCookie pins the pattern backstop directly.
func TestRedactTextHandlesQueryAndCookie(t *testing.T) {
	cases := map[string]string{
		"dsh web: http://127.0.0.1:3080/?token=abc123 ":       "token=" + RedactedMarker,
		"http://host/dsh?foo=1&token=secret&bar=2":            "token=" + RedactedMarker,
		"Set-Cookie: session=deadbeef; Path=/":                "Set-Cookie: " + RedactedMarker,
		"cookie: session=deadbeef":                            "cookie: " + RedactedMarker,
		"nothing sensitive here":                              "nothing sensitive here",
		"a line mentioning tokens generally, without a value": "a line mentioning tokens generally, without a value",
	}
	for input, want := range cases {
		got := RedactText(input)
		if !strings.Contains(got, want) {
			t.Errorf("RedactText(%q) = %q, want it to contain %q", input, got, want)
		}
		if want == "token="+RedactedMarker {
			if strings.Contains(got, "abc123") || strings.Contains(got, "secret") {
				t.Errorf("RedactText(%q) leaked the token value: %q", input, got)
			}
		}
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
