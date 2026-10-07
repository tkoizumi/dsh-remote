package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/tkoizumi/dsh-remote/internal/tailscale"
)

// errNoTerminal reports that there is no controlling terminal to ask on.
var errNoTerminal = errors.New("no controlling terminal")

// offerOperatorGrant asks whether to run the one-time
// `tailscale set --operator` command, and runs it if the answer is yes.
//
// The question is asked on the controlling terminal (/dev/tty), never on
// stdin, so a redirected stdin cannot turn this into an unattended privilege
// escalation. When there is no terminal -- most importantly under the systemd
// service -- it returns errNoTerminal and the caller prints the command for the
// user to run instead of attempting sudo.
func offerOperatorGrant(ctx context.Context) (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, errNoTerminal
	}
	defer tty.Close()

	name := tailscale.OperatorName()
	fmt.Fprintf(tty, "\nTailscale Serve needs root or an operator.\nRun `sudo tailscale set --operator=%s` now? [y/N] ", name)
	answer, _ := bufio.NewReader(tty).ReadString('\n')
	if !confirmYes(answer) {
		return false, nil
	}

	// Run sudo with the terminal as stdin so it can prompt for a password even
	// when this process's own stdin is redirected.
	cmd := exec.CommandContext(ctx, "sudo", "tailscale", "set", "--operator="+name)
	cmd.Stdin = tty
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("`sudo tailscale set --operator=%s` failed: %w", name, err)
	}
	return true, nil
}

// confirmYes reports whether a prompt answer means yes. The default is no:
// an empty answer or anything unrecognised declines.
func confirmYes(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
