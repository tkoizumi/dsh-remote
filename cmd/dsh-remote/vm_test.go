package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureLimaWithoutBrewSuggestsHomebrew(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := ensureLima(context.Background())
	if err == nil {
		t.Fatal("ensureLima succeeded without limactl or brew")
	}
	if !strings.Contains(err.Error(), "brew.sh") {
		t.Fatalf("error should point at Homebrew, got: %v", err)
	}
}

func TestEnsureLimaUsesExistingBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "limactl")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	client, err := ensureLima(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if client.Bin != bin {
		t.Fatalf("Bin = %q, want %q", client.Bin, bin)
	}
}
