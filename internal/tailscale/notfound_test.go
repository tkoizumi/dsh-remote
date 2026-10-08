package tailscale

import (
	"strings"
	"testing"
)

// TestNewMentionsNoServe guards the escape hatch for hosts without Tailscale,
// such as a Linux VM. The message is the only hint a user gets before they
// discover --no-serve in the README.
func TestNewMentionsNoServe(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := New()
	if err == nil {
		t.Fatal("New() succeeded with an empty PATH")
	}
	if !strings.Contains(err.Error(), "--no-serve") {
		t.Fatalf("error should mention --no-serve, got: %v", err)
	}
}
