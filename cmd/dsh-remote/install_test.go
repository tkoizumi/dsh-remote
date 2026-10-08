package main

import (
	"strings"
	"testing"
)

func TestInstallStartFlagsNoServe(t *testing.T) {
	if got := installStartFlags(false, "", true, "", nil); got != " --no-serve" {
		t.Fatalf("installStartFlags = %q, want %q", got, " --no-serve")
	}
}

func TestInstallStartFlagsDshExec(t *testing.T) {
	if got := installStartFlags(false, "", false, "/usr/bin/dsh", nil); got != " --dsh-exec=/usr/bin/dsh" {
		t.Fatalf("installStartFlags = %q", got)
	}
}

func TestInstallStartFlagsTrustedHosts(t *testing.T) {
	got := installStartFlags(false, "", true, "", []string{"mac.ts.net", "192.168.64.5"})
	if want := " --no-serve --trusted-host=mac.ts.net --trusted-host=192.168.64.5"; got != want {
		t.Fatalf("installStartFlags = %q, want %q", got, want)
	}
}

func TestInstallStartFlagsCombines(t *testing.T) {
	got := installStartFlags(true, "192.168.64.5", true, "/usr/bin/dsh", nil)
	for _, want := range []string{" --lan", " --lan-address=192.168.64.5", " --no-serve", " --dsh-exec=/usr/bin/dsh"} {
		if !strings.Contains(got, want) {
			t.Fatalf("installStartFlags = %q, missing %q", got, want)
		}
	}
}

func TestInstallStartFlagsQuotesDSHExecWithSpaces(t *testing.T) {
	got := installStartFlags(false, "", false, "/opt/my tools/dsh", nil)
	if want := ` --dsh-exec="/opt/my tools/dsh"`; got != want {
		t.Fatalf("installStartFlags = %q, want %q", got, want)
	}
}

func TestRenderUnitNoServeDropsTailscaled(t *testing.T) {
	unit := renderUnit("/usr/local/bin/dsh-remote", " --no-serve", true)
	if strings.Contains(unit, "tailscaled") {
		t.Fatalf("a --no-serve unit should not order after tailscaled:\n%s", unit)
	}
	if !strings.Contains(unit, "ExecStart=/usr/local/bin/dsh-remote start --no-serve") {
		t.Fatalf("unit is missing the ExecStart line:\n%s", unit)
	}
	// The intent must survive into the service environment so diagnostics run
	// inside the unit do not report an unmanaged Serve mapping as a fault.
	if !strings.Contains(unit, "Environment="+noServeEnv+"=1") {
		t.Fatalf("a --no-serve unit should record that intent in the environment:\n%s", unit)
	}
}

func TestRenderUnitDefaultOrdersAfterTailscaled(t *testing.T) {
	unit := renderUnit("/usr/local/bin/dsh-remote", "", false)
	if !strings.Contains(unit, "After=network-online.target tailscaled.service") {
		t.Fatalf("unit should order after tailscaled by default:\n%s", unit)
	}
	if strings.Contains(unit, noServeEnv) {
		t.Fatalf("a managed-Serve unit must not set %s:\n%s", noServeEnv, unit)
	}
}

// TestRenderUnitRestartPolicy pins the P0 requirement from the incident report:
// the generated unit must actually restart the proxy after an unexpected exit.
func TestRenderUnitRestartPolicy(t *testing.T) {
	unit := renderUnit("/usr/local/bin/dsh-remote", "", false)
	for _, want := range []string{"Restart=on-failure", "RestartSec=3", "WantedBy=default.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit is missing %q:\n%s", want, unit)
		}
	}
	// Type=simple with a service that exits non-zero is what makes Restart work.
	if !strings.Contains(unit, "Type=simple") {
		t.Errorf("unit is missing Type=simple:\n%s", unit)
	}
}

func TestSystemdArgLeavesPlainValuesAlone(t *testing.T) {
	if got := systemdArg("/usr/bin/dsh"); got != "/usr/bin/dsh" {
		t.Fatalf("systemdArg = %q", got)
	}
}
