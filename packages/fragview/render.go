package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Muted per-class colours for the detailed map mode. Nothing here competes
// with the hostage highlight.
var classColor = [nrClasses]string{
	clNone:     "\x1b[38;5;234m",
	clFree:     "\x1b[38;5;238m",
	clFile:     "\x1b[38;5;31m",
	clShmem:    "\x1b[38;5;37m",
	clAnon:     "\x1b[38;5;65m",
	clABD:      "\x1b[38;5;96m",
	clSlab:     "\x1b[38;5;136m",
	clPgtab:    "\x1b[38;5;97m",
	clCompound: "\x1b[38;5;131m",
	clUncached: "\x1b[38;5;130m",
	clKernel:   "\x1b[38;5;95m",
	clReserved: "\x1b[38;5;242m",
	clUnknown:  "\x1b[38;5;245m",
}

var barOrder = []class{clFree, clFile, clShmem, clAnon, clABD, clSlab,
	clCompound, clUncached, clKernel, clPgtab, clReserved, clUnknown}

var fillGlyph = []rune{'·', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// Returns the complete quantity including its unit. Callers used to append
// "iB" themselves, which turned a plain zero into "0BiB".
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	names := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(b)
	i := -1
	for v >= unit && i < len(names)-1 {
		v /= unit
		i++
	}
	switch {
	case v >= 100:
		return fmt.Sprintf("%.0f%s", v, names[i])
	case v >= 10:
		return fmt.Sprintf("%.1f%s", v, names[i])
	}
	return fmt.Sprintf("%.2f%s", v, names[i])
}

func humanCount(n uint64) string {
	switch {
	case n >= 1000000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

type blockView struct {
	valid   bool
	occ     float64
	pick    class
	movable bool
	hostage bool
}

// A block is a hostage when nearly all of it is free and something immovable
// is keeping the allocator from handing it out anyway.
func summarise(c [nrClasses]uint16) blockView {
	known := uint64(0)
	for i := clFree; i < nrClasses; i++ {
		known += uint64(c[i])
	}
	if known == 0 {
		return blockView{}
	}

	used := known - uint64(c[clFree])
	immovable := uint64(0)
	pick := clFree
	for i := clFree + 1; i < nrClasses; i++ {
		if !i.immovable() {
			continue
		}
		immovable += uint64(c[i])
		if c[i] > c[pick] || !pick.immovable() {
			pick = i
		}
	}
	if immovable == 0 {
		pick = clFree
		for _, m := range []class{clAnon, clShmem, clFile} {
			if c[m] > c[pick] {
				pick = m
			}
		}
	}

	occ := float64(used) / float64(known)
	return blockView{
		valid:   true,
		occ:     occ,
		pick:    pick,
		movable: immovable == 0,
		hostage: immovable > 0 && occ < 0.125,
	}
}

// One screen cell can cover many blocks. Averaging would hide a single hostage
// among twenty healthy neighbours, so a hostage anywhere in the run wins.
func cellFor(views []blockView, from, to int, byClass bool) string {
	sumOcc, n := 0.0, 0
	var counted [nrClasses]int
	movable := 0
	worst := blockView{}

	for i := from; i < to && i < len(views); i++ {
		v := views[i]
		if !v.valid {
			continue
		}
		n++
		sumOcc += v.occ
		counted[v.pick]++
		if v.movable {
			movable++
		}
		if v.hostage && (!worst.hostage || v.occ < worst.occ) {
			worst = v
		}
	}
	if n == 0 {
		return " "
	}

	v := worst
	if !v.hostage {
		v.occ = sumOcc / float64(n)
		v.movable = movable*2 > n
		v.pick = clFree
		for c := clFree; c < nrClasses; c++ {
			if counted[c] > counted[v.pick] {
				v.pick = c
			}
		}
	}

	idx := int(v.occ * 8.999)
	if idx == 0 && v.occ > 0 {
		idx = 1
	}
	if idx > 8 {
		idx = 8
	}

	var colour string
	switch {
	case v.hostage:
		colour = cHostage
	case byClass:
		colour = classColor[v.pick]
	case v.pick == clFree:
		colour = pinFree
	case v.movable:
		colour = pinMovable
	default:
		colour = pinImmovable
	}
	return colour + string(fillGlyph[idx]) + cReset
}

type whoStat struct {
	blocks        uint64
	hostageBlocks uint64
	pages         uint64
	hostagePages  uint64
	mobile        bool
}

type whoTotals struct {
	blocks           uint64
	hostageBlocks    uint64
	hostageFreePages uint64
	walkUS           uint64
}

type frame struct {
	m             *blockMap
	pagesPerBlock uint64
	ladder        freeLadder
	types         blockTypes
	slabs         []slabCache
	mem           map[string]uint64
	vm            map[string]uint64
	arc           map[string]uint64
	who           map[string]whoStat
	whoTot        whoTotals
	whoErr        string
	host          string
	snapshot      string
	kernel        string
	taken         string
	scanned       float64
	sweepMS       int64
	share         int
	interval      time.Duration
	paused        bool
	byClass       bool
	pixels        bool
	px            pixmap
	layout        pixLayout
	pxKey         pxKey
	txAt          time.Time
	gen           uint64
	mapTop        int
	hoverCol      int
	hoverRow      int
	hoverFrom     int
	hoverTo       int
	boxFrom       int
	boxTo         int

	views       []blockView
	hostages    int
	hostageFree uint64
	viewFrom    int
	viewTo      int

	// Published by the last render so the key handler can scroll in the
	// units the user actually sees rather than in raw block counts.
	rowBlocks    int
	screenBlocks int

	focusSlab  bool
	slabSort   int
	slabScroll int
	slabRows   int
	slabTotal  int
}

// Which caches matter depends on the question being asked, so the order is a
// setting rather than a decision.
var slabSorts = []struct {
	name string
	less func(a, b slabCache, wa, wb whoStat) bool
}{
	{"hostage pages", func(a, b slabCache, wa, wb whoStat) bool {
		if wa.hostagePages != wb.hostagePages {
			return wa.hostagePages > wb.hostagePages
		}
		return wa.blocks > wb.blocks
	}},
	{"blocks touched", func(a, b slabCache, wa, wb whoStat) bool {
		if wa.blocks != wb.blocks {
			return wa.blocks > wb.blocks
		}
		return wa.hostagePages > wb.hostagePages
	}},
	{"least dense", func(a, b slabCache, wa, wb whoStat) bool {
		// A cache that is mostly holes wastes the blocks it sits in, but one
		// with three live objects is noise. Rank by the memory the holes add
		// up to rather than by the percentage alone.
		return a.wasted() > b.wasted()
	}},
	{"size", func(a, b slabCache, wa, wb whoStat) bool {
		return a.bytes() > b.bytes()
	}},
}

func (f *frame) sortName() string { return slabSorts[f.slabSort%len(slabSorts)].name }

func (f *frame) sortedSlabs() []slabCache {
	out := make([]slabCache, 0, len(f.slabs))
	for _, c := range f.slabs {
		if f.who != nil && f.who[c.name].blocks == 0 {
			continue
		}
		out = append(out, c)
	}
	less := slabSorts[f.slabSort%len(slabSorts)].less
	sort.SliceStable(out, func(i, j int) bool {
		return less(out[i], out[j], f.who[out[i].name], f.who[out[j].name])
	})
	return out
}

func occStyle(occ float64) lipgloss.Style {
	switch {
	case occ < 0.5:
		return badStyle
	case occ < 0.8:
		return warnStyle
	}
	return lipgloss.NewStyle()
}

func moveMark(mobile bool) string {
	if mobile {
		return goodStyle.Render("    ✓")
	}
	return faint("    -")
}

// The whole table, scrollable, for when fourteen rows of it are not enough.
func (f *frame) slabFullPane(rows int) []string {
	all := f.sortedSlabs()
	f.slabTotal = len(all)
	f.slabRows = max(rows-2, 1)

	if f.slabScroll > len(all)-f.slabRows {
		f.slabScroll = len(all) - f.slabRows
	}
	if f.slabScroll < 0 {
		f.slabScroll = 0
	}

	shown := faint("   no per-block data, load slabwho.ko")
	if f.who != nil {
		shown = faint(fmt.Sprintf("   showing %d–%d of %d", f.slabScroll+1,
			min(f.slabScroll+f.slabRows, len(all)), len(all)))
	}
	out := []string{
		head("slab caches") + faint("   order: ") +
			boldStyle.Render(f.sortName()) + faint(" (o cycles)") + shown,
		faint(fmt.Sprintf(" %-28s %10s %10s %8s %6s %8s %8s %10s %5s",
			"cache", "size", "wasted", "objsize", "used", "blocks", "hostage", "host.pages", "move")),
	}

	for i := f.slabScroll; i < len(all) && i < f.slabScroll+f.slabRows; i++ {
		c := all[i]
		w := f.who[c.name]
		out = append(out, fmt.Sprintf(" %-28s %10s %10s %8s %s %8s %8s %10s %s",
			trunc(c.name, 28), humanBytes(c.bytes()), humanBytes(c.wasted()),
			humanBytes(c.objSize),
			occStyle(c.occupancy()).Render(fmt.Sprintf("%5.0f%%", c.occupancy()*100)),
			humanCount(w.blocks), humanCount(w.hostageBlocks), humanCount(w.hostagePages),
			moveMark(w.mobile)))
	}
	return out
}

func (f *frame) summarise() {
	if len(f.views) != f.m.nblocks {
		f.views = make([]blockView, f.m.nblocks)
	}
	f.hostages, f.hostageFree = 0, 0
	for i, c := range f.m.counts {
		v := summarise(c)
		f.views[i] = v
		if v.hostage {
			f.hostages++
			f.hostageFree += uint64(c[clFree])
		}
	}
	if f.viewTo == 0 || f.viewTo > f.m.nblocks {
		f.viewTo = f.m.nblocks
	}
}

// Zooming happens towards whatever the pointer is on: that block keeps its
// place on screen while the rest of memory moves in or out around it. With the
// pointer off the map the top of the window stays put instead, so that zoom-in
// followed by scrolling still walks through memory in one direction.
func (f *frame) zoom(span int) {
	if span < 32 {
		span = 32
	}
	if span > f.m.nblocks {
		span = f.m.nblocks
	}

	if old := f.viewTo - f.viewFrom; f.hoverTo > f.hoverFrom && old > 0 {
		at := float64(f.hoverFrom-f.viewFrom) / float64(old)
		f.viewFrom = f.hoverFrom - int(at*float64(span))
	}
	f.viewTo = f.viewFrom + span
	f.clamp()
}

func (f *frame) pan(by int) {
	f.viewFrom += by
	f.viewTo += by
	f.clamp()
}

func (f *frame) clamp() {
	// The box belongs to a window, not to a block. Once the window moves it is
	// pointing at whatever now happens to occupy that place, so it goes; the
	// pointer puts it back where it belongs on the next settle.
	f.boxFrom, f.boxTo = 0, 0

	span := f.viewTo - f.viewFrom
	if f.viewFrom < 0 {
		f.viewFrom = 0
	}
	f.viewTo = f.viewFrom + span
	if f.viewTo > f.m.nblocks {
		f.viewTo = f.m.nblocks
		f.viewFrom = f.viewTo - span
		if f.viewFrom < 0 {
			f.viewFrom = 0
		}
	}
}

func (f *frame) render(rows, cols int) string {
	f.summarise()

	w := cols - 1
	if w < 60 {
		w = 60
	}

	top := []string{f.titleLine(w), rule(w)}
	top = append(top, f.compositionLines(w)...)
	top = append(top, rule(w), f.verdictLine(), rule(w))
	top = append(top, f.ladderLines()...)
	top = append(top, rule(w))

	avail := rows - len(top) - 3

	// There are several hundred caches and only a dozen rows for them beside
	// the map, so the table gets a mode of its own where it takes the lot.
	if f.focusSlab {
		var b strings.Builder
		for _, l := range top {
			b.WriteString(l + "\n")
		}
		for _, l := range f.slabFullPane(avail + 2) {
			b.WriteString(l + "\n")
		}
		b.WriteString(f.footerLine())
		return b.String()
	}

	// Otherwise the map is the point of the tool, so it gets first claim on
	// the space that is left and the cache list is truncated to fit. Whatever
	// the map does not need, because it has run out of memory to draw, goes
	// back to the tables instead of being left blank.
	// Which block the pointer is over is worked out from the layout the last
	// frame used, because this frame's layout is not known until the panes
	// beneath the map have been sized, and they are what the answer goes into.
	f.resolveHover()

	left := f.leftPane()
	tableRows := min(max(avail/2, 3), max(len(left), 18))
	mapRows := max(avail-tableRows, 1)

	// A character cell cannot show less than one block, so zooming past that
	// point can only give the map fewer rows and hand the space to the tables.
	// The pixel renderer has no such floor: it keeps its height and spends the
	// zoom on making each block wider, which is what zooming in is for.
	mapWidth := max(w-mapLabel, 16)
	span := f.viewTo - f.viewFrom
	per := max((span+mapWidth*mapRows-1)/(mapWidth*mapRows), 1)
	if needed := (span + mapWidth*per - 1) / (mapWidth * per); needed < mapRows && !f.pixels {
		tableRows += mapRows - needed
		mapRows = needed
	}

	bottom := append([]string{rule(w)}, joinPanes(left, f.slabPane(tableRows-1), 40)...)
	if len(bottom) > tableRows+1 {
		bottom = bottom[:tableRows+1]
	}

	var b strings.Builder
	b.Grow(1 << 18)
	for _, l := range top {
		b.WriteString(l + "\n")
	}
	f.mapTop = len(top) + 1
	if f.pixels {
		f.pixelBlockMap(&b, w, mapRows)
	} else {
		f.blockMap(&b, w, mapRows)
	}
	for _, l := range bottom {
		b.WriteString(l + "\n")
	}
	b.WriteString(f.footerLine())
	return b.String()
}

func (f *frame) titleLine(w int) string {
	// The blocks span more address space than the machine has memory: the
	// range from the first page frame to the last takes in the PCI hole and
	// every other gap. Report what the kernel says is actually there.
	span := uint64(f.m.nblocks) * f.pagesPerBlock * pageSize
	total := f.mem["MemTotal"]
	if total == 0 {
		total = span
	}

	period := time.Duration(f.share) * f.interval
	right := fmt.Sprintf("map refresh %s · sweep %dms", period.Round(100*time.Millisecond), f.sweepMS)
	if f.whoTot.walkUS > 0 {
		right += fmt.Sprintf(" · walk %dms", f.whoTot.walkUS/1000)
	}
	right = faint(right)
	if f.paused {
		right = warnStyle.Render("PAUSED") + faint(" · ") + right
	}
	// A snapshot has no refresh rate and no sweep time. What it does have is a
	// name and the kernel it came off, which is the whole point of comparing two.
	if f.snapshot != "" {
		right = faint(f.kernel)
		if f.taken != "" {
			right += faint(" · " + f.taken)
		}
	}

	who := f.host
	if f.snapshot != "" {
		who = f.host + " " + boldStyle.Render(f.snapshot)
	}
	left := titleStyle.Render("fragview") + fmt.Sprintf("  %s  %s RAM · %d pageblocks × %s over %s",
		who, humanBytes(total), f.m.nblocks, humanBytes(f.pagesPerBlock*pageSize),
		humanBytes(span))
	return padVisible(left, w-lipgloss.Width(right)) + right
}

func (f *frame) compositionLines(w int) []string {
	var sum [nrClasses]uint64
	for _, c := range f.m.counts {
		for i := range c {
			sum[i] += uint64(c[i])
		}
	}
	known := uint64(0)
	for c := clFree; c < nrClasses; c++ {
		known += sum[c]
	}
	if known == 0 {
		return []string{"", ""}
	}

	legend := " "
	for _, c := range barOrder {
		if sum[c]*200 < known {
			continue
		}
		legend += fmt.Sprintf("%s█%s %s %s   ", classColor[c], cReset,
			classNames[c], humanBytes(sum[c]*pageSize))
	}

	bar, drawn, width := " ", 0, w-2
	for _, c := range barOrder {
		n := int(float64(sum[c]) / float64(known) * float64(width))
		if n == 0 && sum[c] > 0 {
			n = 1
		}
		if drawn+n > width {
			n = width - drawn
		}
		bar += classColor[c] + strings.Repeat("█", n) + cReset
		drawn += n
	}
	return []string{legend, bar}
}

// The one line that says whether this machine can serve a high order
// allocation right now, and what is stopping it.
func (f *frame) verdictLine() string {
	hostages, locked := f.hostages, f.hostageFree
	if f.whoTot.blocks > 0 {
		hostages, locked = int(f.whoTot.hostageBlocks), f.whoTot.hostageFreePages
	}

	ok, fail := f.vm["compact_success"], f.vm["compact_fail"]
	rate := 0.0
	if ok+fail > 0 {
		rate = float64(fail) / float64(ok+fail) * 100
	}
	rateStyle := goodStyle
	if rate > 50 {
		rateStyle = badStyle
	} else if rate > 20 {
		rateStyle = warnStyle
	}

	return fmt.Sprintf(" %s pinning %s   %s  compaction %s ok / %s failed %s   kcompactd woke %s",
		hostageStyle.Render(fmt.Sprintf("%d hostage blocks", hostages)),
		boldStyle.Render(humanBytes(locked*pageSize)),
		faint("│"),
		goodStyle.Render(humanCount(ok)), badStyle.Render(humanCount(fail)),
		rateStyle.Render(fmt.Sprintf("(%.0f%%)", rate)),
		humanCount(f.vm["compact_daemon_wake"]))
}

// The free lists as a horizontal strip: three short rows read far better than
// eleven tall ones, and the shape of the ladder is the whole point.
func (f *frame) ladderLines() []string {
	ui := f.ladder.unusableIndex()
	orders := faint(" free lists    order")
	counts := faint("              blocks")
	unus := faint("            unusable")

	for o := 0; o < len(f.ladder.orders); o++ {
		frac := 0.0
		if ui != nil {
			frac = ui[o]
		}
		st := lipgloss.NewStyle()
		if frac > 0.9 {
			st = badStyle
		} else if frac > 0.5 {
			st = warnStyle
		}
		orders += st.Render(fmt.Sprintf("%6d", o))
		counts += fmt.Sprintf("%6s", humanCount(f.ladder.orders[o]))
		unus += st.Render(fmt.Sprintf("%5.0f%%", frac*100))
	}
	return []string{orders, counts, unus}
}

const mapLabel = 6

func (f *frame) blockMap(b *strings.Builder, w, mapRows int) {
	width := max(w-mapLabel, 16)
	span := f.viewTo - f.viewFrom
	per := max((span+width*mapRows-1)/(width*mapRows), 1)

	mode := "movable or pinned"
	if f.byClass {
		mode = "owner"
	}

	f.rowBlocks = width * per
	f.screenBlocks = f.rowBlocks * mapRows

	blockBytes := f.pagesPerBlock * pageSize
	scope := faint(fmt.Sprintf(" · showing all %s", humanBytes(uint64(f.m.nblocks)*blockBytes)))
	if span < f.m.nblocks {
		scope = fmt.Sprintf(" · %s %s–%s of %s", boldStyle.Render("window"),
			humanBytes(uint64(f.viewFrom)*blockBytes), humanBytes(uint64(f.viewTo)*blockBytes),
			humanBytes(uint64(f.m.nblocks)*blockBytes))
	}
	b.WriteString(faint(fmt.Sprintf(" map · cell = %d block(s) = %s · height = used · colour = %s",
		per, humanBytes(uint64(per)*blockBytes), mode)) + scope +
		faint(" · ") + hostageStyle.Render("red = hostage") + "\n")

	idx := f.viewFrom
	for r := 0; r < mapRows; r++ {
		at := uint64(f.viewFrom+r*width*per) * f.pagesPerBlock * pageSize
		b.WriteString(faint(fmt.Sprintf("%4.1fG ", float64(at)/(1<<30))))
		for c := 0; c < width; c++ {
			if idx >= f.viewTo {
				b.WriteString(" ")
				continue
			}
			b.WriteString(cellFor(f.views, idx, idx+per, f.byClass))
			idx += per
		}
		b.WriteString("\n")
	}
}

func (f *frame) leftPane() []string {
	if f.hoverTo > f.hoverFrom {
		return f.hoverPane()
	}

	out := []string{head("pageblocks by migrate type")}
	for i, name := range f.types.names {
		if f.types.counts[i] == 0 {
			continue
		}
		out = append(out, fmt.Sprintf(" %-14s %10s", name, humanCount(f.types.counts[i])))
	}

	out = append(out, "", head("page flags vs kernel counters"))
	return append(out, f.reconcile()...)
}

// Page flags and the kernel's own counters are independent accounts of the
// same memory. Where they disagree the difference is real information.
func (f *frame) reconcile() []string {
	var sum [nrClasses]uint64
	for _, c := range f.m.counts {
		for i := range c {
			sum[i] += uint64(c[i])
		}
	}
	mine := func(c class) uint64 { return sum[c] * pageSize }

	rows := []struct {
		label         string
		flags, kernel uint64
	}{
		{"free", mine(clFree), f.mem["MemFree"]},
		{"shmem", mine(clShmem), f.mem["Shmem"]},
		{"anon", mine(clAnon), f.mem["AnonPages"]},
		{"cache", mine(clFile), f.mem["Cached"] - f.mem["Shmem"]},
		{"slab", mine(clSlab), f.mem["Slab"]},
		// Scatter ABD carries both halves of the ARC; only buffers small enough
		// for the zio_buf_comb caches land in slab instead.
		{classNames[clABD], mine(clABD), f.arc["data_size"] + f.arc["metadata_size"]},
		{"pgtable", mine(clPgtab), f.mem["PageTables"] + f.mem["SecPageTables"]},
	}

	out := []string{faint(fmt.Sprintf(" %-11s %8s %9s", "", "flags", "kernel"))}
	for _, r := range rows {
		if r.flags == 0 && r.kernel == 0 {
			continue
		}
		mark := ""
		if r.kernel > 0 {
			if d := float64(r.flags)/float64(r.kernel) - 1; d > 0.05 || d < -0.05 {
				mark = " " + warnStyle.Render(fmt.Sprintf("%+.0f%%", d*100))
			}
		}
		out = append(out, fmt.Sprintf(" %-11s %7s %8s%s", r.label,
			humanBytes(r.flags), humanBytes(r.kernel), mark))
	}

	unknown := mine(clKernel) + mine(clCompound) + mine(clUncached) + mine(clUnknown)
	out = append(out, fmt.Sprintf(" %-11s %7s", "unaccounted", humanBytes(unknown)),
		faint(fmt.Sprintf("   vmalloc %s · gpu %s",
			humanBytes(f.mem["VmallocUsed"]), humanBytes(f.mem["GPUActive"]))))

	// Pages carrying flags that classify() has no rule for. This should stay at
	// zero; anything else names the combination that needs a rule.
	if n := mine(clUnknown); n > 0 {
		line := fmt.Sprintf(" %-11s %7s", "unclassified", humanBytes(n))
		if seen := unknownSeen(); len(seen) > 0 {
			line += "  " + faint(flagString(seen[0].flags))
		}
		out = append(out, line)
	}
	return out
}

func (f *frame) slabPane(limit int) []string {
	all := f.sortedSlabs()
	f.slabTotal = len(all)

	if f.who == nil {
		note := "load slabwho.ko for the block columns"
		if f.whoErr != "" {
			note = f.whoErr
		}
		out := []string{head("slab caches") + faint(" · "+f.sortName()), faint(" " + note),
			faint(fmt.Sprintf(" %-22s %9s %6s", "cache", "size", "used"))}
		for i, c := range all {
			if i >= limit-3 {
				break
			}
			out = append(out, fmt.Sprintf(" %-22s %8s %5.0f%%",
				trunc(c.name, 22), humanBytes(c.bytes()), c.occupancy()*100))
		}
		return out
	}

	out := []string{
		head("slab caches") + faint(" by "+f.sortName()) +
			faint(fmt.Sprintf("   %d more with Tab", max(len(all)-limit+2, 0))),
		faint(fmt.Sprintf(" %-22s %9s %5s %7s %7s %5s",
			"cache", "size", "used", "blocks", "hostage", "move"))}

	for _, c := range all {
		if len(out) >= limit {
			break
		}
		w := f.who[c.name]
		out = append(out, fmt.Sprintf(" %-22s %8s %s %7s %7s%s",
			trunc(c.name, 22), humanBytes(c.bytes()),
			occStyle(c.occupancy()).Render(fmt.Sprintf("%4.0f%%", c.occupancy()*100)),
			humanCount(w.blocks), humanCount(w.hostageBlocks), moveMark(w.mobile)))
	}
	return out
}

func (f *frame) footerLine() string {
	if f.focusSlab {
		return faint(" q quit · Tab back to map · o sort order · ↑↓ scroll · PgUp/PgDn page · space pause")
	}
	nav := "↑↓ scroll · PgUp/PgDn page · g whole map"
	if f.viewTo-f.viewFrom >= f.m.nblocks {
		nav = "↑↓ scroll (zoom in first) · g whole map"
	}
	// The colour mode is a choice the character map has to make and the pixel
	// map does not, so offering it there would be offering nothing.
	mode := "m colour · p pixels"
	if f.pixels {
		mode = "p character map · hover for detail"
	}
	if f.snapshot != "" {
		return faint(" q quit · Tab slab table · " + mode + " · z/Z or ctrl+wheel zoom · " + nav)
	}
	return faint(" q quit · Tab slab table · space pause · c compact · " + mode + " · z/Z or ctrl+wheel zoom · " +
		nav + " · +/- refresh rate")
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// The wheel walks the map a row at a time, or zooms when held with control,
// which is the pair of meanings every other program gives it.
func (f *frame) wheel(down, ctrl bool) {
	if f.focusSlab {
		step := 3
		if !down {
			step = -step
		}
		f.slabScroll += step
		return
	}

	span := f.viewTo - f.viewFrom
	switch {
	case ctrl && down:
		f.zoom(span * 2)
	case ctrl:
		f.zoom(span / 2)
	case down:
		f.pan(max(f.rowBlocks, 1))
	default:
		f.pan(-max(f.rowBlocks, 1))
	}
}
