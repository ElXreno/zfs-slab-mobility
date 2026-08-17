package main

import (
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestPlaceholderWidth(t *testing.T) {
	var b strings.Builder
	placeholderRow(&b, 3, 20)
	s := b.String()

	if w := ansi.StringWidth(s); w != 20 {
		t.Fatalf("placeholder row measures %d columns, expected 20", w)
	}
	if got := ansi.Truncate(s, 20, ""); got != s {
		t.Fatalf("truncating to 20 columns changed the row: %d bytes became %d", len(s), len(got))
	}
	t.Logf("20 cells = %d bytes, width %d", len(s), ansi.StringWidth(s))
}

// Renders a synthetic map to a PNG so the drawing can be looked at without a
// terminal. The blocks are built to cover every case the renderer has to get
// right: pure classes, a slab-plus-ARC mix, an almost empty block with one
// immovable page in it, and a hole.
func TestPaintPixels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks int
	}{{"max-zoom", 32}, {"zoomed-in", 240}, {"zoomed-out", 24000}} {
		t.Run(tc.name, func(t *testing.T) { paintCase(t, tc.name, tc.blocks) })
	}
}

func paintCase(t *testing.T, name string, blocks int) {
	const ppb = 512

	m := &blockMap{nblocks: blocks, counts: make([][nrClasses]uint16, blocks)}
	for i := range m.counts {
		var c [nrClasses]uint16
		switch i % 8 {
		case 0:
			c[clFree] = ppb
		case 1:
			c[clAnon], c[clFree] = ppb*3/4, ppb/4
		case 2:
			c[clSlab], c[clFree] = ppb/2, ppb/2
		case 3:
			c[clSlab], c[clABD], c[clFree] = ppb/3, ppb/3, ppb-2*(ppb/3)
		case 4:
			c[clFile], c[clShmem], c[clFree] = ppb/2, ppb/8, ppb-ppb/2-ppb/8
		case 5:
			c[clSlab], c[clFree] = 8, ppb-8
		case 6:
			c[clKernel], c[clPgtab], c[clUnknown], c[clFree] = 100, 40, 12, ppb-152
		case 7:
			// a hole in the memory map
		}
		m.counts[i] = c
	}

	f := &frame{m: m, pagesPerBlock: uint64(ppb), viewTo: blocks}
	f.summarise()

	hostages := 0
	for _, v := range f.views {
		if v.hostage {
			hostages++
		}
	}
	if hostages == 0 {
		t.Fatal("no hostage blocks at all: nothing to outline")
	}

	l := f.paintPixels(200, 27, 9, 19)

	out, err := os.Create("/tmp/fragview-" + name + ".png")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := png.Encode(out, f.px.img); err != nil {
		t.Fatal(err)
	}
	t.Logf("%dx%d, tile %dx%d, %d per row x %d rows, %d hostage -> %s",
		f.px.img.Rect.Dx(), f.px.img.Rect.Dy(), l.blockPx, l.rowSpan*l.cellH,
		l.blocksPerRow, l.tileRows, hostages, out.Name())
}

// Realistic size: 62G of memory is about 31000 pageblocks, drawn across a
// 200-cell wide map twelve rows tall on a 9x19 font.
func BenchmarkPaintPixels(b *testing.B) {
	const blocks = 31000
	m := &blockMap{nblocks: blocks, counts: make([][nrClasses]uint16, blocks)}
	for i := range m.counts {
		m.counts[i][clFree] = uint16(512 - i%400)
		m.counts[i][clSlab] = uint16(i % 400 / 2)
		m.counts[i][clABD] = uint16(i % 400 / 2)
	}
	f := &frame{m: m, pagesPerBlock: 512, viewTo: blocks}
	f.summarise()

	for b.Loop() {
		f.paintPixels(200, 12, 9, 19)
	}
}

// The pointer has to land on the block that was drawn where the pointer is.
// Painting and hit testing are two separate pieces of arithmetic over the same
// layout, which is exactly the arrangement that drifts apart unmentioned.
func TestHoverRoundTrip(t *testing.T) {
	const (
		blocks = 600
		width  = 200
		rows   = 27
		cellW  = 9
		cellH  = 19
	)

	m := &blockMap{nblocks: blocks, counts: make([][nrClasses]uint16, blocks)}
	for i := range m.counts {
		m.counts[i][clFree] = 512
	}
	f := &frame{m: m, pagesPerBlock: 512, viewTo: blocks, pixels: true, mapTop: 12}
	f.summarise()
	l := f.paintPixels(width, rows, cellW, cellH)

	if l.blockPx == 0 {
		t.Fatalf("need a layout with blocks wider than a pixel, got %+v", l)
	}

	checked := 0
	for tile := 0; tile < l.tileRows; tile++ {
		for j := 0; j < l.blocksPerRow; j++ {
			want := f.viewFrom + tile*l.blocksPerRow + j
			if want >= f.viewTo {
				continue
			}
			x0, x1 := l.blockSpan(j)
			top, _ := l.rowSpanY(tile)

			f.hoverCol = mapLabel + (x0+x1)/2/cellW
			f.hoverRow = f.mapTop + top/cellH
			f.resolveHover()

			if f.hoverFrom != want || f.hoverTo != want+1 {
				t.Fatalf("block %d (row %d, slot %d, pixels %d-%d): hover returned %d-%d",
					want, tile, j, x0, x1, f.hoverFrom, f.hoverTo)
			}
			checked++
		}
	}
	t.Logf("tile %dx%d, %d blocks checked", l.blockPx, l.rowSpan*l.cellH, checked)
}
