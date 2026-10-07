// Package qr renders a URL as a terminal QR code.
//
// It draws from the QR bitmap with explicit black/white background colours
// instead of relying on the terminal's default foreground: a QR code is only
// scannable when the polarity is right, and "dark glyph on light default
// background" inverts on the dark terminals most phones' users have.
package qr

import (
	"bytes"
	"fmt"
	"io"

	qrcode "github.com/skip2/go-qrcode"
)

// ANSI SGR sequences forcing the module background colour.
const (
	blackCell = "\x1b[40m "
	whiteCell = "\x1b[47m "
	reset     = "\x1b[0m"
)

// Render writes a scannable QR code for content to w.
func Render(w io.Writer, content string) error {
	bitmap, err := Bitmap(content)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, row := range bitmap {
		for _, dark := range row {
			if dark {
				buf.WriteString(blackCell)
			} else {
				buf.WriteString(whiteCell)
			}
		}
		buf.WriteString(reset)
		buf.WriteByte('\n')
	}
	_, err = w.Write(buf.Bytes())
	return err
}

// Bitmap returns the QR module grid, including its quiet-zone border, where
// true marks a dark module. It exists so the renderer's polarity can be tested
// without parsing escape sequences.
func Bitmap(content string) ([][]bool, error) {
	code, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return nil, fmt.Errorf("encode QR code: %w", err)
	}
	return code.Bitmap(), nil
}
