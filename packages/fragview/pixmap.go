// Drawing the block map as pixels instead of characters.
//
// A character cell can carry one colour, so the text map has to pick a single
// class per block and throw the rest away. That is how a block half full of
// slab and half full of ARC data spent months looking like a slab block. Here
// every block is drawn as a stack of coloured bands in the proportions the scan
// actually found, so a mixed block looks mixed.
//
// The image goes out over the kitty graphics protocol, which is an APC escape
// carrying base64. Sending it separately from the frame keeps the huge data
// escape away from bubbletea's per-line diffing; what lands in the frame is a
// grid of Unicode placeholder cells that tell the terminal where to put it.
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const placeholderRune = "\U0010EEEE"

// Any id will do as long as it is stable and not zero; the map is the only
// image this program ever sends.
const pixmapImageID = 0x00F2A6

// Shortest gap between two pictures. Twenty five a second is smooth to look at
// and well inside what the terminal can inflate and upload.
const minRedraw = 40 * time.Millisecond

// Bottom to top. The immovable classes go at the bottom because they are what
// the block is being judged on, free space at the top because that is the part
// the allocator wanted and did not get.
var stackOrder = []class{clSlab, clABD, clKernel, clUncached, clPgtab, clCompound,
	clReserved, clUnknown, clAnon, clShmem, clFile, clFree}

var (
	classRGB  [nrClasses]color.RGBA
	hostagePx = color.RGBA{255, 95, 95, 255}
	voidPx    = color.RGBA{0, 0, 0, 255}
)

// The two renderers have to agree on what a colour means, so the pixel palette
// is derived from the escape sequences the legend already uses rather than
// written out a second time.
func init() {
	for c := class(0); c < nrClasses; c++ {
		classRGB[c] = xterm256RGB(paletteIndex(classColor[c]))
	}
}

func paletteIndex(esc string) int {
	_, rest, ok := strings.Cut(esc, "38;5;")
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSuffix(rest, "m"))
	return n
}

func xterm256RGB(idx int) color.RGBA {
	switch {
	case idx >= 232:
		v := uint8(8 + (idx-232)*10)
		return color.RGBA{v, v, v, 255}
	case idx >= 16:
		lv := [6]uint8{0, 95, 135, 175, 215, 255}
		i := idx - 16
		return color.RGBA{lv[i/36], lv[(i/6)%6], lv[i%6], 255}
	}
	v := uint8(128)
	if idx >= 8 {
		v = 255
	}
	return color.RGBA{v * uint8(idx&1), v * uint8((idx>>1)&1), v * uint8((idx>>2)&1), 255}
}

type winsize struct{ row, col, xpixel, ypixel uint16 }

// Terminal cell size in pixels. Kitty fills in the pixel fields of the window
// size ioctl; anything that does not gets a guess, and the map is merely
// stretched rather than wrong.
func cellPixels() (int, int) {
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(),
		uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.col == 0 || ws.row == 0 || ws.xpixel == 0 || ws.ypixel == 0 {
		return 8, 16
	}
	return int(ws.xpixel) / int(ws.col), int(ws.ypixel) / int(ws.row)
}

func kittyAvailable() bool {
	if os.Getenv("FRAGVIEW_PIXELS") == "0" {
		return false
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return true
	}
	return strings.Contains(os.Getenv("TERM"), "kitty")
}

// A pixel map is redrawn from scratch every frame, so the image and the encode
// buffers are kept rather than reallocated sixteen megabytes at a time.
type pixmap struct {
	img  *image.RGBA
	sent []byte
	raw  bytes.Buffer
	out  bytes.Buffer
}

func (p *pixmap) canvas(w, h int) *image.RGBA {
	if p.img == nil || p.img.Rect.Dx() != w || p.img.Rect.Dy() != h {
		p.img = image.NewRGBA(image.Rect(0, 0, w, h))
		p.sent = nil
	}
	return p.img
}

func (p *pixmap) set(x, y int, c color.RGBA) {
	if !(image.Point{x, y}.In(p.img.Rect)) {
		return
	}
	i := p.img.PixOffset(x, y)
	p.img.Pix[i], p.img.Pix[i+1], p.img.Pix[i+2], p.img.Pix[i+3] = c.R, c.G, c.B, 255
}

// Transmit the image and place it as a virtual placement, which is what the
// placeholder cells in the frame refer to. Both the image id and the placement
// id are fixed, so every frame replaces the last instead of piling up.
// Builds the transmit and placement escapes for the current picture, or nothing
// at all when it has not changed. The map only moves when a scan finds
// something different, and re-sending four megabytes of identical pixels
// several times a second is the sort of thing that gets blamed on the terminal.
func (p *pixmap) encode(cols, rows int) bool {
	if bytes.Equal(p.sent, p.img.Pix) {
		p.out.Reset()
		return false
	}
	p.sent = append(p.sent[:0], p.img.Pix...)

	p.raw.Reset()
	zw, _ := zlib.NewWriterLevel(&p.raw, zlib.BestSpeed)
	zw.Write(p.img.Pix)
	zw.Close()

	enc := base64.StdEncoding.EncodeToString(p.raw.Bytes())
	p.out.Reset()

	const chunk = 4096
	first := true
	for len(enc) > 0 {
		n := min(chunk, len(enc))
		piece := enc[:n]
		enc = enc[n:]
		more := 0
		if len(enc) > 0 {
			more = 1
		}
		if first {
			fmt.Fprintf(&p.out, "\x1b_Ga=t,f=32,o=z,s=%d,v=%d,t=d,i=%d,q=2,m=%d;%s\x1b\\",
				p.img.Rect.Dx(), p.img.Rect.Dy(), pixmapImageID, more, piece)
			first = false
		} else {
			fmt.Fprintf(&p.out, "\x1b_Gm=%d;%s\x1b\\", more, piece)
		}
	}
	fmt.Fprintf(&p.out, "\x1b_Ga=p,U=1,i=%d,p=1,c=%d,r=%d,q=2\x1b\\", pixmapImageID, cols, rows)
	return true
}

// One placeholder row. Only the first cell carries the row and column
// diacritics; the terminal continues the run from there, which keeps the frame
// small and the width count honest at one column per cell.
func placeholderRow(b *strings.Builder, row, cols int) {
	r, g, bl := (pixmapImageID>>16)&0xFF, (pixmapImageID>>8)&0xFF, pixmapImageID&0xFF
	fmt.Fprintf(b, "\x1b[38;2;%d;%d;%dm", r, g, bl)
	b.WriteString(placeholderRune)
	b.WriteRune(diacritic(row))
	b.WriteRune(diacritic(0))
	b.WriteRune(diacritic(pixmapImageID >> 24))
	for i := 1; i < cols; i++ {
		b.WriteString(placeholderRune)
	}
	b.WriteString("\x1b[39m")
}

// How the blocks on screen line up with the pixels available to draw them.
//
// Everything here is integer arithmetic on purpose. The first version scaled
// blocks to pixels with one floating point ratio across the whole map, so a row
// held a fractional number of blocks and every row started a little further
// into a block than the last. That is what put the visible skew in the picture,
// and it made scrolling by a row shift the blocks sideways as well as down.
type pixLayout struct {
	viewFrom     int // the window this layout was painted for
	pxW          int
	cellW, cellH int
	mapRows      int
	tileRows     int // rows of blocks on screen
	rowSpan      int // terminal rows one row of blocks occupies
	blocksPerRow int
	blockPx      int // nominal width of one block, zero when several share a column
	perCol       int // blocks under one pixel column, only when blockPx is zero
	inset        int
}

// Nothing ties a block to a single terminal row: the image is continuous, and
// only the address labels down the side care where the rows are. So zooming in
// grows a block in both directions, taking as many terminal rows per row of
// blocks as it needs to keep the tile from turning into a strip.
const maxTileAspect = 2

func pixLayoutFor(span, pxW, cellW, cellH, mapRows int) pixLayout {
	l := pixLayout{pxW: pxW, cellW: cellW, cellH: cellH,
		mapRows: mapRows, tileRows: mapRows, rowSpan: 1}

	if perRow := max((span+mapRows-1)/mapRows, 1); perRow > pxW {
		l.perCol = (perRow + pxW - 1) / pxW
		l.blocksPerRow = l.perCol * pxW
		return l
	}

	for l.rowSpan = 1; l.rowSpan <= mapRows; l.rowSpan++ {
		l.tileRows = mapRows / l.rowSpan
		l.blocksPerRow = max((span+l.tileRows-1)/l.tileRows, 1)
		l.blockPx = pxW / l.blocksPerRow
		if l.blockPx <= maxTileAspect*l.rowSpan*cellH || l.tileRows == 1 {
			break
		}
	}

	switch h := l.rowSpan * cellH; {
	case l.blockPx >= 8 && h >= 12:
		l.inset = 2
	case l.blockPx >= 4:
		l.inset = 1
	}
	return l
}

// Pixel range of the j-th block of a row. Dividing into the width rather than
// multiplying a block width out keeps every row identical and spends the
// leftover pixels evenly instead of leaving a gap at the right edge.
func (l pixLayout) blockSpan(j int) (int, int) {
	return j * l.pxW / l.blocksPerRow, (j+1)*l.pxW/l.blocksPerRow - 1
}

func (l pixLayout) rowSpanY(t int) (int, int) {
	return t * l.rowSpan * l.cellH, (t+1)*l.rowSpan*l.cellH - 1
}

// Everything the picture depends on. Repainting and re-sending four megabytes
// costs seven milliseconds here and a texture upload in the terminal, which the
// pointer would otherwise pay on every cell it crosses — that is what dragged a
// tail behind the mouse. Now a frame that would draw the same picture draws
// nothing at all.
type pxKey struct {
	from, to     int
	width, rows  int
	cellW, cellH int
	gen          uint64
	hoverFrom    int
	hoverTo      int
}

func (f *frame) pixelBlockMap(b *strings.Builder, w, mapRows int) {
	width := max(w-mapLabel, 16)
	cellW, cellH := cellPixels()

	key := pxKey{from: f.viewFrom, to: f.viewTo, width: width, rows: mapRows,
		cellW: cellW, cellH: cellH, gen: f.gen}
	key.hoverFrom, key.hoverTo = f.boxFrom, f.boxTo
	// Four megabytes of picture per frame is more than the terminal will take:
	// it has to inflate and re-upload the whole thing, and once its input backs
	// up our own write blocks, input queues, and a burst of scrolling lands long
	// after the wheel stopped. So the picture is rate limited and the input is
	// not; a skipped frame is followed by catchUpMsg.
	f.px.out.Reset()
	if key != f.pxKey && time.Since(f.txAt) >= minRedraw {
		f.pxKey = key
		f.txAt = time.Now()
		f.paintPixels(width, mapRows, cellW, cellH)
		f.px.encode(width, mapRows)
	}
	l := f.layout

	// The picture rides inside the frame rather than going to the terminal on
	// its own. Writing it straight to stdout from here raced the renderer's own
	// goroutine, which writes under a lock we do not hold, so a fifty kilobyte
	// escape could land in the middle of one of its sequences. That was the map
	// flickering for no reason anyone could name.
	b.Write(f.px.out.Bytes())
	f.pixelHeader(b, l)
	for r := 0; r < mapRows; r++ {
		// One label per row of blocks, not per terminal row, or the addresses
		// would claim a precision the picture does not have.
		if r%l.rowSpan == 0 {
			// Off the layout's own window, not the current one: the labels
			// annotate the picture on screen, which may be a frame behind.
			at := uint64(l.viewFrom+r/l.rowSpan*l.blocksPerRow) * f.pagesPerBlock * pageSize
			b.WriteString(faint(fmt.Sprintf("%4.1fG ", float64(at)/(1<<30))))
		} else {
			b.WriteString(strings.Repeat(" ", mapLabel))
		}
		placeholderRow(b, r, width)
		b.WriteString("\n")
	}
}

func (f *frame) paintPixels(width, mapRows, cellW, cellH int) pixLayout {
	pxW, pxH := width*cellW, mapRows*cellH
	l := pixLayoutFor(f.viewTo-f.viewFrom, pxW, cellW, cellH, mapRows)

	f.rowBlocks = l.blocksPerRow
	f.screenBlocks = l.blocksPerRow * l.tileRows

	img := f.px.canvas(pxW, pxH)
	for i := range img.Pix {
		img.Pix[i] = 0
	}

	l.viewFrom = f.viewFrom
	if l.blockPx == 0 {
		f.paintAggregated(l)
	} else {
		f.paintBlocks(l)
	}
	f.markHover(l)

	f.layout = l
	return l
}

// One block, one tile. Wide enough and the tile is drawn inside its own edges,
// which leaves a gap to the next tile and room for the hostage box to sit
// around the contents rather than on top of them.
func (f *frame) paintBlocks(l pixLayout) {
	for t := 0; t < l.tileRows; t++ {
		top, bottom := l.rowSpanY(t)
		for j := 0; j < l.blocksPerRow; j++ {
			blk := f.viewFrom + t*l.blocksPerRow + j
			if blk >= f.viewTo || blk >= len(f.m.counts) {
				return
			}

			var sum [nrClasses]uint64
			known := uint64(0)
			for c := clFree; c < nrClasses; c++ {
				sum[c] = uint64(f.m.counts[blk][c])
				known += sum[c]
			}
			if known == 0 {
				continue
			}

			x0, x1 := l.blockSpan(j)
			f.stack(sum, known, x0+l.inset, x1-l.inset, top+l.inset, bottom-l.inset)
			if blk < len(f.views) && f.views[blk].hostage {
				f.box(x0, x1, top, bottom, hostagePx)
			}
		}
	}
}

// Zoomed out past one pixel per block the counts of everything under a column
// are added together, and a hostage can no longer be drawn as a shape. It
// becomes brightness instead: the top pixel of the column carries the share of
// the blocks under it that are held hostage. Outlining each one would paint the
// map solid red, since a third of this machine's blocks qualify.
func (f *frame) paintAggregated(l pixLayout) {
	for r := 0; r < l.mapRows; r++ {
		for x := 0; x < l.pxW; x++ {
			lo := f.viewFrom + r*l.blocksPerRow + x*l.perCol
			hi := min(lo+l.perCol, f.viewTo)

			var sum [nrClasses]uint64
			known, held, n := uint64(0), 0, 0
			for blk := lo; blk < hi && blk < len(f.m.counts); blk++ {
				for c := clFree; c < nrClasses; c++ {
					sum[c] += uint64(f.m.counts[blk][c])
					known += uint64(f.m.counts[blk][c])
				}
				if blk < len(f.views) && f.views[blk].valid {
					n++
					if f.views[blk].hostage {
						held++
					}
				}
			}
			if known == 0 {
				continue
			}

			top, bottom := r*l.cellH, (r+1)*l.cellH-1
			f.stack(sum, known, x, x, top, bottom)
			if held > 0 {
				f.px.set(x, top, fade(hostagePx, float64(held)/float64(n)))
			}
		}
	}
}

func (f *frame) stack(sum [nrClasses]uint64, known uint64, x0, x1, top, bottom int) {
	h := bottom - top + 1
	if h < 1 || x1 < x0 {
		return
	}

	minBand := 1
	if h >= 10 {
		minBand = 2
	}
	bands := bandHeights(sum, known, h, minBand)

	y := bottom
	for _, c := range stackOrder {
		for i := 0; i < bands[c] && y >= top; i++ {
			for x := x0; x <= x1; x++ {
				f.px.set(x, y, classRGB[c])
			}
			y--
		}
	}
}

func (f *frame) box(x0, x1, top, bottom int, c color.RGBA) {
	for x := x0; x <= x1; x++ {
		f.px.set(x, top, c)
		f.px.set(x, bottom, c)
	}
	for y := top; y <= bottom; y++ {
		f.px.set(x0, y, c)
		f.px.set(x1, y, c)
	}
}

// Page counts to pixel band heights adding up to exactly h.
//
// A class that is present at all gets at least minBand pixels. Proportion alone
// would give a cache holding eight pages of a block a single pixel, and a one
// pixel line inside a twenty pixel tile reads as an artefact rather than as
// information — when the small classes are precisely the ones this tool exists
// to find.
func bandHeights(sum [nrClasses]uint64, known uint64, h, minBand int) [nrClasses]int {
	var out [nrClasses]int
	type share struct {
		c    class
		frac float64
	}
	var fracs []share

	total := 0
	for _, c := range stackOrder {
		if sum[c] == 0 {
			continue
		}
		exact := float64(sum[c]) / float64(known) * float64(h)
		n := max(int(exact), minBand)
		out[c] = n
		total += n
		fracs = append(fracs, share{c, exact - float64(int(exact))})
	}
	if len(fracs) == 0 {
		return out
	}

	// The minimum can overshoot when a block holds more classes than the tile
	// has pixels. Pay for it out of the tallest bands, which can spare it.
	for total > h {
		tallest, tallestN := class(0), 0
		for _, c := range stackOrder {
			if out[c] > minBand && out[c] > tallestN {
				tallest, tallestN = c, out[c]
			}
		}
		if tallestN == 0 {
			for _, c := range stackOrder {
				if out[c] > 0 {
					out[c]--
					total--
					break
				}
			}
			continue
		}
		out[tallest]--
		total--
	}

	sort.Slice(fracs, func(i, j int) bool { return fracs[i].frac > fracs[j].frac })
	for i := 0; total < h; i, total = i+1, total+1 {
		out[fracs[i%len(fracs)].c]++
	}
	return out
}

func fade(c color.RGBA, frac float64) color.RGBA {
	if frac > 1 {
		frac = 1
	}
	mix := func(v uint8) uint8 { return uint8(float64(v) * (0.25 + 0.75*frac)) }
	return color.RGBA{mix(c.R), mix(c.G), mix(c.B), 255}
}

func (f *frame) pixelHeader(b *strings.Builder, l pixLayout) {
	blockBytes := f.pagesPerBlock * pageSize
	span := f.viewTo - f.viewFrom

	unit, mark := fmt.Sprintf("%d block(s) per column", l.perCol), "red line = hostage density"
	if l.blockPx > 0 {
		unit, mark = fmt.Sprintf("%d×%d px", l.blockPx, l.rowSpan*l.cellH), "red outline = hostage"
	}

	scope := faint(fmt.Sprintf(" · showing all %s", humanBytes(uint64(f.m.nblocks)*blockBytes)))
	if span < f.m.nblocks {
		scope = fmt.Sprintf(" · %s %s–%s of %s", boldStyle.Render("window"),
			humanBytes(uint64(f.viewFrom)*blockBytes), humanBytes(uint64(f.viewTo)*blockBytes),
			humanBytes(uint64(f.m.nblocks)*blockBytes))
	}

	b.WriteString(faint(fmt.Sprintf(" map · pixels · block = %s · %d per row · stacked by owner, free on top",
		unit, l.blocksPerRow)) + scope + faint(" · ") +
		hostageStyle.Render(mark) + "\n")
}

// The 297 combining marks the kitty protocol uses to number placeholder rows
// and columns, in the order the specification lists them.
var diacritics = []rune{
	0x0305, 0x030D, 0x030E, 0x0310, 0x0312, 0x033D, 0x033E, 0x033F, 0x0346, 0x034A,
	0x034B, 0x034C, 0x0350, 0x0351, 0x0352, 0x0357, 0x035B, 0x0363, 0x0364, 0x0365,
	0x0366, 0x0367, 0x0368, 0x0369, 0x036A, 0x036B, 0x036C, 0x036D, 0x036E, 0x036F,
	0x0483, 0x0484, 0x0485, 0x0486, 0x0487, 0x0592, 0x0593, 0x0594, 0x0595, 0x0597,
	0x0598, 0x0599, 0x059C, 0x059D, 0x059E, 0x059F, 0x05A0, 0x05A1, 0x05A8, 0x05A9,
	0x05AB, 0x05AC, 0x05AF, 0x05C4, 0x0610, 0x0611, 0x0612, 0x0613, 0x0614, 0x0615,
	0x0616, 0x0617, 0x0653, 0x0654, 0x0657, 0x0658, 0x0659, 0x065A, 0x065B, 0x065D,
	0x065E, 0x06D6, 0x06D7, 0x06D8, 0x06D9, 0x06DA, 0x06DB, 0x06DC, 0x06DF, 0x06E0,
	0x06E1, 0x06E2, 0x06E4, 0x06E7, 0x06E8, 0x06EB, 0x06EC, 0x0730, 0x0732, 0x0733,
	0x0735, 0x0736, 0x073A, 0x073D, 0x073F, 0x0741, 0x0743, 0x0745, 0x0747, 0x0749,
	0x074A, 0x07EB, 0x07EC, 0x07ED, 0x07EE, 0x07EF, 0x07F0, 0x07F1, 0x07F2, 0x07F3,
	0x0816, 0x0817, 0x0818, 0x0819, 0x081B, 0x081C, 0x081D, 0x081E, 0x081F, 0x0820,
	0x0821, 0x0822, 0x0823, 0x0825, 0x0826, 0x0827, 0x0829, 0x082A, 0x082B, 0x082C,
	0x082D, 0x0951, 0x0952, 0x0953, 0x0954, 0x0F82, 0x0F83, 0x0F86, 0x0F87, 0x135D,
	0x135E, 0x135F, 0x17DD, 0x193A, 0x1A17, 0x1A75, 0x1A76, 0x1A77, 0x1A78, 0x1A79,
	0x1A7A, 0x1A7B, 0x1A7C, 0x1AB0, 0x1AB1, 0x1AB2, 0x1AB3, 0x1AB4, 0x1AB5, 0x1AB6,
	0x1AB7, 0x1AB8, 0x1AB9, 0x1ABA, 0x1ABB, 0x1ABC, 0x1ABD, 0x1B6B, 0x1B6C, 0x1B6D,
	0x1B6E, 0x1B6F, 0x1B70, 0x1B71, 0x1B72, 0x1B73, 0x1CD0, 0x1CD1, 0x1CD2, 0x1CD4,
	0x1CD5, 0x1CD6, 0x1CD7, 0x1CD8, 0x1CD9, 0x1CDA, 0x1CDB, 0x1CDC, 0x1CDD, 0x1CDE,
	0x1CDF, 0x1CE0, 0x1CE2, 0x1CE3, 0x1CE4, 0x1CE5, 0x1CE6, 0x1CE7, 0x1CE8, 0x1CED,
	0x1CF4, 0x1CF8, 0x1CF9, 0x1DC0, 0x1DC1, 0x1DC2, 0x1DC3, 0x1DC4, 0x1DC5, 0x1DC6,
	0x1DC7, 0x1DC8, 0x1DC9, 0x1DCA, 0x1DCB, 0x1DCC, 0x1DCD, 0x1DCE, 0x1DCF, 0x1DD0,
	0x1DD1, 0x1DD2, 0x1DD3, 0x1DD4, 0x1DD5, 0x1DD6, 0x1DD7, 0x1DD8, 0x1DD9, 0x1DDA,
	0x1DDB, 0x1DDC, 0x1DDD, 0x1DDE, 0x1DDF, 0x1DE0, 0x1DE1, 0x1DE2, 0x1DE3, 0x1DE4,
	0x1DE5, 0x1DE6, 0x1DE7, 0x1DE8, 0x1DE9, 0x1DEA, 0x1DEB, 0x1DEC, 0x1DED, 0x1DEE,
	0x1DEF, 0x1DF0, 0x1DF1, 0x1DF2, 0x1DF3, 0x1DF4, 0x1DF5, 0x1DFB, 0x1DFC, 0x1DFD,
	0x1DFE, 0x1DFF, 0x20D0, 0x20D1, 0x20D2, 0x20D3, 0x20D4, 0x20D5, 0x20D6, 0x20D7,
	0x20D8, 0x20D9, 0x20DA, 0x20DB, 0x20DC, 0x20E1, 0x20E5, 0x20E6, 0x20E7, 0x20E8,
	0x20E9, 0x20EA, 0x20EB, 0x20EC, 0x20ED, 0x20EE, 0x20EF, 0x20F0, 0x2CEF, 0x2CF0,
	0x2CF1, 0x2D7F, 0x2DE0, 0x2DE1, 0x2DE2, 0x2DE3, 0x2DE4, 0x2DE5, 0x2DE6, 0x2DE7,
	0x2DE8, 0x2DE9, 0x2DEA, 0x2DEB, 0x2DEC, 0x2DED, 0x2DEE, 0x2DEF, 0x2DF0, 0x2DF1,
	0x2DF2, 0x2DF3, 0x2DF4, 0x2DF5, 0x2DF6, 0x2DF7, 0x2DF8, 0x2DF9, 0x2DFA, 0x2DFB,
	0x2DFC, 0x2DFD, 0x2DFE, 0x2DFF, 0x302A, 0x302B, 0x302C, 0x302D, 0x3099, 0x309A,
	0xA66F, 0xA674, 0xA675, 0xA676, 0xA677, 0xA678, 0xA679, 0xA67A, 0xA67B, 0xA67C,
	0xA67D, 0xA69E, 0xA69F, 0xA6F0, 0xA6F1, 0xA806, 0xA8C4, 0xA8E0, 0xA8E1, 0xA8E2,
	0xA8E3, 0xA8E4, 0xA8E5, 0xA8E6, 0xA8E7, 0xA8E8, 0xA8E9, 0xA8EA, 0xA8EB, 0xA8EC,
	0xA8ED, 0xA8EE, 0xA8EF, 0xA8F0, 0xA8F1, 0xAAB0, 0xAAB2, 0xAAB3, 0xAAB4, 0xAAB7,
	0xAAB8, 0xAABE, 0xAABF, 0xAAC1, 0xAAEC, 0xAAED, 0xAAF6, 0xABE5, 0xABE8, 0xABED,
	0xFB1E, 0xFE00, 0xFE01, 0xFE02, 0xFE03, 0xFE04, 0xFE05, 0xFE06, 0xFE07, 0xFE08,
	0xFE09, 0xFE0A, 0xFE0B, 0xFE0C, 0xFE0D, 0xFE0E, 0xFE0F, 0xFE20, 0xFE21, 0xFE22,
	0xFE23, 0xFE24, 0xFE25, 0xFE26, 0xFE27, 0xFE28, 0xFE29, 0xFE2A, 0xFE2B, 0xFE2C,
	0xFE2D, 0xFE2E, 0xFE2F, 0x101FD, 0x102E0, 0x10376, 0x10377, 0x10378, 0x10379,
	0x1037A, 0x10A0D, 0x10A0F, 0x10A38, 0x10A39, 0x10A3A, 0x10A3F, 0x10AE5, 0x10AE6,
	0x11001, 0x11038, 0x11039, 0x1103A, 0x1103B, 0x1103C, 0x1103D, 0x1103E, 0x1103F,
	0x11040, 0x11041, 0x11042, 0x11043, 0x11044, 0x11045, 0x11046, 0x1107F, 0x110B3,
	0x110B4, 0x110B5, 0x110B6, 0x110B9, 0x110BA, 0x11100, 0x11101, 0x11102, 0x11127,
	0x11128, 0x11129, 0x1112A, 0x1112B, 0x1112D, 0x1112E, 0x1112F, 0x11130, 0x11131,
	0x11132, 0x11133, 0x11134, 0x11173, 0x11180, 0x11181, 0x111B6, 0x111B7, 0x111B8,
	0x111B9, 0x111BA, 0x111BB, 0x111BC, 0x111BD, 0x111BE, 0x111CA, 0x111CB, 0x111CC,
	0x1122F, 0x11230, 0x11231, 0x11234, 0x11236, 0x11237, 0x1123E, 0x112DF, 0x112E3,
	0x112E4, 0x112E5, 0x112E6, 0x112E7, 0x112E8, 0x112E9, 0x112EA, 0x11300, 0x11301,
	0x1133C, 0x1133E, 0x11340, 0x11366, 0x11367, 0x11368, 0x11369, 0x1136A, 0x1136B,
	0x1136C, 0x11370, 0x11371, 0x11372, 0x11373, 0x11374, 0x11438, 0x11439, 0x1143A,
	0x1143B, 0x1143C, 0x1143D, 0x1143E, 0x1143F, 0x11442, 0x11443, 0x11444, 0x11446,
	0x114B3, 0x114B4, 0x114B5, 0x114B6, 0x114B7, 0x114B8, 0x114BA, 0x114BF, 0x114C0,
	0x114C2, 0x114C3, 0x115B2, 0x115B3, 0x115B4, 0x115B5, 0x115BC, 0x115BD, 0x115BF,
	0x115C0, 0x115DC, 0x115DD, 0x11633, 0x11634, 0x11635, 0x11636, 0x11637, 0x11638,
	0x11639, 0x1163A, 0x1163D, 0x1163F, 0x11640, 0x116AB, 0x116AD, 0x116B0, 0x116B1,
	0x116B2, 0x116B3, 0x116B4, 0x116B5, 0x116B7, 0x1171D, 0x1171E, 0x1171F, 0x11722,
	0x11723, 0x11724, 0x11725, 0x11727, 0x11728, 0x11729, 0x1172A, 0x1172B, 0x11C30,
	0x11C31, 0x11C32, 0x11C33, 0x11C34, 0x11C35, 0x11C36, 0x11C38, 0x11C39, 0x11C3A,
	0x11C3B, 0x11C3C, 0x11C3D, 0x11C3F, 0x11C92, 0x11C93, 0x11C94, 0x11C95, 0x11C96,
	0x11C97, 0x11C98, 0x11C99, 0x11C9A, 0x11C9B, 0x11C9C, 0x11C9D, 0x11C9E, 0x11C9F,
	0x11CA0, 0x11CA1, 0x11CA2, 0x11CA3, 0x11CA4, 0x11CA5, 0x11CA6, 0x11CA7, 0x11CAA,
	0x11CAB, 0x11CAC, 0x11CAD, 0x11CAE, 0x11CAF, 0x11CB0, 0x11CB2, 0x11CB3, 0x11CB5,
	0x11CB6, 0x16AF0, 0x16AF1, 0x16AF2, 0x16AF3, 0x16AF4, 0x16B30, 0x16B31, 0x16B32,
	0x16B33, 0x16B34, 0x16B35, 0x16B36, 0x1BC9D, 0x1BC9E, 0x1D167, 0x1D168, 0x1D169,
	0x1D17B, 0x1D17C, 0x1D17D, 0x1D17E, 0x1D17F, 0x1D180, 0x1D181, 0x1D182, 0x1D185,
	0x1D186, 0x1D187, 0x1D188, 0x1D189, 0x1D18A, 0x1D18B, 0x1D1AA, 0x1D1AB, 0x1D1AC,
	0x1D1AD, 0x1D242, 0x1D243, 0x1D244, 0x1E000, 0x1E001, 0x1E002, 0x1E003, 0x1E004,
	0x1E005, 0x1E006, 0x1E008, 0x1E009, 0x1E00A, 0x1E00B, 0x1E00C, 0x1E00D, 0x1E00E,
	0x1E00F, 0x1E010, 0x1E011, 0x1E012, 0x1E013, 0x1E014, 0x1E015, 0x1E016, 0x1E017,
	0x1E018, 0x1E01B, 0x1E01C, 0x1E01D, 0x1E01E, 0x1E01F, 0x1E020, 0x1E021, 0x1E023,
	0x1E024, 0x1E026, 0x1E027, 0x1E028, 0x1E029, 0x1E02A, 0x1E8D0, 0x1E8D1, 0x1E8D2,
	0x1E8D3, 0x1E8D4, 0x1E8D5, 0x1E8D6,
}

func diacritic(pos int) rune {
	if pos < 0 || pos >= len(diacritics) {
		return diacritics[0]
	}
	return diacritics[pos]
}

// Which blocks the pointer is over, in the layout the previous frame drew.
//
// The terminal reports the pointer in cells, so the answer is a range: one
// block when a block is bigger than a cell, and everything under the cell when
// it is not. Reporting a single block in the second case would be a guess
// dressed up as a fact.
func (f *frame) resolveHover() {
	f.hoverFrom, f.hoverTo = 0, 0

	l := f.layout
	if !f.pixels || l.blocksPerRow == 0 || f.mapTop == 0 {
		return
	}
	col, row := f.hoverCol-mapLabel, f.hoverRow-f.mapTop
	if col < 0 || row < 0 || row >= l.mapRows {
		return
	}

	x := col * l.cellW
	if x >= l.pxW {
		return
	}
	t := row * l.cellH / (l.rowSpan * l.cellH)
	if t >= l.tileRows {
		return
	}

	from, to := 0, 0
	if l.blockPx > 0 {
		from = (x + l.cellW/2) * l.blocksPerRow / l.pxW
		to = from + 1
	} else {
		from = x * l.perCol
		to = min(x+l.cellW, l.pxW) * l.perCol
	}

	// Against the window the layout was painted for, not the current one: after
	// a zoom the two differ for a frame, and reading new bounds off an old grid
	// would put the pointer on a block that was never drawn there.
	last := min(l.viewFrom+l.tileRows*l.blocksPerRow, f.m.nblocks)
	f.hoverFrom = min(l.viewFrom+t*l.blocksPerRow+from, last)
	f.hoverTo = min(l.viewFrom+t*l.blocksPerRow+to, last)
}

var hoverPx = color.RGBA{255, 255, 255, 255}

// The block the pointer came to rest on, boxed in white. Drawn last so it wins
// over the hostage box: while the pointer is there the panes below say
// everything the red box was there to hint at.
//
// It is drawn where the pointer settled rather than where the pointer is, so
// that wandering about inside one block changes nothing. Following the pointer
// exactly meant redrawing the picture on every cell crossed, which showed up as
// the box blinking as the mouse moved within its own outline.
func (f *frame) markHover(l pixLayout) {
	if f.boxTo <= f.boxFrom {
		return
	}
	i := f.boxFrom - l.viewFrom
	if i < 0 {
		return
	}
	t := i / l.blocksPerRow
	if t >= l.tileRows {
		return
	}

	top, bottom := l.rowSpanY(t)
	j := i % l.blocksPerRow
	if l.blockPx > 0 {
		x0, x1 := l.blockSpan(j)
		f.box(x0, x1, top, bottom, hoverPx)
		return
	}
	f.box(j/l.perCol, (f.boxTo-l.viewFrom-1-t*l.blocksPerRow)/l.perCol, top, bottom, hoverPx)
}

func (f *frame) hoverPane() []string {
	var sum [nrClasses]uint64
	known, immovable := uint64(0), uint64(0)
	for blk := f.hoverFrom; blk < f.hoverTo && blk < len(f.m.counts); blk++ {
		for c := clFree; c < nrClasses; c++ {
			sum[c] += uint64(f.m.counts[blk][c])
			known += uint64(f.m.counts[blk][c])
			if c.immovable() {
				immovable += uint64(f.m.counts[blk][c])
			}
		}
	}

	blockBytes := f.pagesPerBlock * pageSize
	what := fmt.Sprintf("block %d", f.hoverFrom)
	if f.hoverTo-f.hoverFrom > 1 {
		what = fmt.Sprintf("blocks %d–%d", f.hoverFrom, f.hoverTo-1)
	}
	out := []string{
		head("under the pointer") + faint("   "+what),
		faint(fmt.Sprintf(" %s–%s", humanBytes(uint64(f.hoverFrom)*blockBytes),
			humanBytes(uint64(f.hoverTo)*blockBytes))),
	}
	if known == 0 {
		return append(out, faint(" not memory: a hole in the map"))
	}

	used := known - sum[clFree]
	state := goodStyle.Render("free to compact")
	switch {
	case f.hoverFrom < len(f.views) && f.views[f.hoverFrom].hostage:
		state = hostageStyle.Render("hostage")
	case immovable > 0:
		state = warnStyle.Render("pinned")
	}
	out = append(out, fmt.Sprintf(" %s used · %s immovable · %s",
		boldStyle.Render(fmt.Sprintf("%.0f%%", float64(used)/float64(known)*100)),
		humanBytes(immovable*pageSize), state), "")

	for _, c := range barOrder {
		if sum[c] == 0 {
			continue
		}
		out = append(out, fmt.Sprintf(" %s█%s %-9s %6d pages %9s", classColor[c], cReset,
			classNames[c], sum[c], humanBytes(sum[c]*pageSize)))
	}
	return out
}
