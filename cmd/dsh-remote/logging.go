package main

import (
	"io"

	"github.com/tkoizumi/dsh-remote/internal/logging"
)

// openLog opens the persistent lifecycle log for commands that are not the
// supervised proxy itself. It returns nil when the path cannot be resolved,
// because a diagnostic or a stop must never fail just because it cannot log.
//
// The caller owns closing the result.
func openLog(sink io.Writer) *logging.Logger {
	path, err := logging.Path()
	if err != nil {
		return nil
	}
	return logging.Open(path, sink)
}

// logPathBestEffort resolves the log path for reporting, or "" when it cannot
// be resolved.
func logPathBestEffort() string {
	path, err := logging.Path()
	if err != nil {
		return ""
	}
	return path
}
