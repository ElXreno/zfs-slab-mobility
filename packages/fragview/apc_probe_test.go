package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The transmit escape has to be able to travel inside a frame line. If the
// renderer's width arithmetic counted any of it, the line would be truncated
// mid-payload and the terminal would be fed a broken image.
func TestTransmitEscapeIsZeroWidth(t *testing.T) {
	p := realisticImage(t)
	p.encode(200, 27)
	esc := p.out.String()

	if len(esc) < 10000 {
		t.Fatalf("expected a long sequence, got %d bytes", len(esc))
	}
	if w := ansi.StringWidth(esc); w != 0 {
		t.Fatalf("transmit sequence measures %d columns, must be 0", w)
	}

	line := esc + " 4.1G " + strings.Repeat(placeholderRune, 60)
	if got := ansi.Truncate(line, 66, ""); got != line {
		t.Fatalf("truncating to 66 columns ate %d bytes of %d", len(line)-len(got), len(line))
	}
	t.Logf("%d bytes of escape, zero width, truncation leaves it alone", len(esc))
}
