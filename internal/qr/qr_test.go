package qr

import (
	"bytes"
	"strings"
	"testing"
)

// TestBitmapHasValidFinderPattern pins the polarity: the top-left finder
// pattern is dark-ringed, and the four-module quiet zone around it is light.
func TestBitmapHasValidFinderPattern(t *testing.T) {
	bitmap, err := Bitmap("https://ubuntu-server.tail6d6db9.ts.net/dsh")
	if err != nil {
		t.Fatal(err)
	}
	if len(bitmap) < 11 || len(bitmap[0]) < 11 {
		t.Fatalf("bitmap too small: %dx%d", len(bitmap), len(bitmap[0]))
	}
	// Quiet zone.
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			if bitmap[y][x] {
				t.Fatalf("quiet zone module (%d,%d) is dark; the code would be unscannable", y, x)
			}
		}
	}
	// Finder pattern starts at (4,4): a 7x7 dark ring with a light inner ring.
	for x := 4; x < 11; x++ {
		if !bitmap[4][x] {
			t.Fatalf("top row of the finder pattern is not dark at x=%d", x)
		}
	}
	for x := 5; x < 10; x++ {
		if bitmap[5][x] {
			t.Fatalf("ring gap at (5,%d) should be light", x)
		}
	}
}

func TestRenderEmitsBlackAndWhiteCells(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, "https://example.ts.net/dsh"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, blackCell) {
		t.Fatal("no dark modules rendered")
	}
	if !strings.Contains(out, whiteCell) {
		t.Fatal("no light modules rendered")
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("output should end with a newline")
	}
	// Every rendered row must be reset so the terminal is left clean.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i, line := range lines {
		if !strings.HasSuffix(line, reset) {
			t.Fatalf("row %d does not end with a reset sequence", i)
		}
	}
}
