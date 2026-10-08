package logging

import (
	"regexp"
	"strings"
)

// RedactedMarker replaces anything recognised as a credential.
const RedactedMarker = "REDACTED"

var (
	// tokenQueryRE matches a token query parameter, keeping the parameter name
	// so the shape of the line stays readable.
	tokenQueryRE = regexp.MustCompile(`(?i)([?&]token=)[^&\s"']+`)
	// cookieHeaderRE matches the value of a Set-Cookie or Cookie header.
	cookieHeaderRE = regexp.MustCompile(`(?i)((?:set-)?cookie\s*:\s*)[^\r\n]+`)
	// secretAssignmentRE matches "label: value" or "label=value" for the words
	// that name a credential, so a line that prints the token after naming it is
	// covered even when the value is not known in advance. A separator is
	// required: prose that merely mentions "token" must stay readable.
	secretAssignmentRE = regexp.MustCompile(`(?i)\b(token|secret|password|passwd|passphrase|api[_-]?key|authorization|bearer|credential)s?\b(\s*[:=]\s*)([^\s,;]+)`)
)

// RedactText removes credential-shaped substrings from a log line without
// needing to know the token in advance.
//
// This is the backstop, not the primary defence: the token is removed by value
// before the line is logged (see dsh.Process.RedactingWriter), because a random
// credential is not distinguishable from ordinary text by shape alone. What this
// adds is coverage for the lines that arrive before the token has been parsed,
// or from a code path that never saw it.
func RedactText(line string) string {
	line = tokenQueryRE.ReplaceAllString(line, "${1}"+RedactedMarker)
	line = cookieHeaderRE.ReplaceAllString(line, "${1}"+RedactedMarker)
	line = secretAssignmentRE.ReplaceAllString(line, "${1}${2}"+RedactedMarker)
	return line
}

// TokenURLRedacted reports whether a line still appears to carry a bootstrap
// token. It exists so tests can assert the guarantee rather than trust it.
func TokenURLRedacted(line string) bool {
	return !strings.Contains(strings.ToLower(line), "token=") ||
		strings.Contains(line, "token="+RedactedMarker)
}
