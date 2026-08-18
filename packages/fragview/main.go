// fragview — htop for physical memory fragmentation.
//
// It answers the question /proc/buddyinfo only hints at: which pageblocks are
// unusable for a high order allocation, and what exactly is sitting in them.
// Free counters tell you an order-9 block is unavailable; this shows you the
// nearly-empty block and names the slab cache holding it hostage.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func readPageblockOrder() uint64 {
	f, err := openProc("pagetypeinfo")
	if err != nil {
		return 9
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if _, v, ok := strings.Cut(sc.Text(), "Page block order:"); ok {
			if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
				return n
			}
		}
	}
	return 9
}

// Per-cache pageblock accounting, if the slabwho module is loaded. The kernel
// does not export which cache owns a slab page, so without it the block
// columns simply stay empty rather than being guessed at.
func readSlabwho() (map[string]whoStat, whoTotals, string) {
	f, err := openProc("slabwho")
	if err != nil {
		return nil, whoTotals{}, ""
	}
	defer f.Close()

	out := map[string]whoStat{}
	var tot whoTotals
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) == 0 {
			continue
		}
		num := func(i int) uint64 {
			n, _ := strconv.ParseUint(fl[i], 10, 64)
			return n
		}
		if fl[0] == "#" {
			for i := 1; i+1 < len(fl); i += 2 {
				switch fl[i] {
				case "blocks":
					tot.blocks = num(i + 1)
				case "hostage_blocks":
					tot.hostageBlocks = num(i + 1)
				case "hostage_free_pages":
					tot.hostageFreePages = num(i + 1)
				case "walk_us":
					tot.walkUS = num(i + 1)
				}
			}
			continue
		}
		if len(fl) < 6 {
			continue
		}
		out[fl[0]] = whoStat{
			pages:         num(1),
			blocks:        num(2),
			hostagePages:  num(3),
			hostageBlocks: num(4),
			mobile:        fl[5] == "1",
		}
	}
	if len(out) == 0 {
		return nil, whoTotals{}, "/proc/slabwho is empty"
	}
	return out, tot, ""
}

// Allocation sites, from a separate file on a slower kernel clock. It costs a
// fault tolerant read per object rather than per page, so a monitor refreshing
// the map twice a second reads it only while the table that shows it is open.
func readSites() []siteStat {
	f, err := openProc("slabwho_sites")
	if err != nil {
		return nil
	}
	defer f.Close()

	var sites []siteStat
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) < 6 || fl[0] != "@" {
			continue
		}
		num := func(i int) uint64 {
			n, _ := strconv.ParseUint(fl[i], 10, 64)
			return n
		}
		sites = append(sites, siteStat{
			objects: num(1), blocks: num(2),
			cache: fl[3], fn: fl[4], file: fl[5],
		})
	}
	return sites
}

func hostname() string {
	b, err := os.ReadFile(procPath("sys/kernel/hostname"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Scanning runs off the event loop, so everything the view touches is guarded.
// Contention is only ever between one scan and one repaint.
type model struct {
	mu       sync.Mutex
	f        *frame
	ppb      uint64
	interval time.Duration
	cursor   int
	rows     int
	cols     int
	ready    bool
	hoverSeq uint64
	wantFull bool
	scratch  [][nrClasses]uint16
	readBuf  []byte
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

type scanDoneMsg struct{}

type hoverSettleMsg uint64

// Asks for one more frame after the terminal has had a moment. The picture is
// rate limited, so the frame that lands during a burst of scrolling may be the
// one that gets skipped; this makes sure the burst still ends on the truth.
type catchUpMsg struct{}

func catchUp() tea.Cmd {
	return tea.Tick(60*time.Millisecond, func(time.Time) tea.Msg { return catchUpMsg{} })
}

// Everything is read first and published second, so the renderer is never
// waiting on a /proc file. /proc/slabwho alone can take tens of milliseconds
// because it walks every page frame in the machine.
func (m *model) collect() {
	ladder, _ := readBuddyinfo()
	types, _ := readPagetypeinfo()
	slabs, _ := readSlabinfo()
	mem, vm, arc := readMeminfo(), readVmstat(), readARC()
	cacheAliases = loadCacheAliases()
	who, whoTot, whoErr := readSlabwho()

	// Only while the table that shows them is open, so idle monitoring on the
	// map never triggers the expensive per object walk in the module.
	var sites []siteStat
	if m.f.focusSlab {
		sites = readSites()
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.f.ladder, m.f.types, m.f.slabs = ladder, types, slabs
	m.f.mem, m.f.vm, m.f.arc = mem, vm, arc
	m.f.who, m.f.whoTot, m.f.whoErr = who, whoTot, whoErr
	if sites != nil {
		m.f.sites = sites
	}
}

// Scan [from, to) into scratch space and publish it under the lock. The read
// itself is the expensive part and must not block a repaint; the handover is a
// memory copy of a few tens of kilobytes.
func (m *model) scanRange(from, to int) {
	if to > m.f.m.nblocks {
		to = m.f.m.nblocks
	}
	if to <= from {
		return
	}
	if len(m.scratch) < to-from {
		m.scratch = make([][nrClasses]uint16, to-from)
	}
	if m.readBuf == nil {
		m.readBuf = make([]byte, 1<<20)
	}

	start := time.Now()
	err := m.f.m.refreshInto(from, to, m.ppb, m.scratch, m.readBuf)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return
	}

	m.mu.Lock()
	copy(m.f.m.counts[from:to], m.scratch[:to-from])
	m.f.gen++
	m.f.sweepMS = elapsed
	m.f.scanned = float64(to-from) / float64(m.f.m.nblocks)
	m.mu.Unlock()
}

// One slice of the map per tick, or the whole thing when something has asked
// for it. There is exactly one of these in flight at any moment: every
// scanDoneMsg schedules precisely one successor, and nothing else may start a
// second chain. Two scans at once would share the cursor and the scratch
// buffers, and paint one part of the map with pages read from another.
func (m *model) scan() tea.Msg {
	m.mu.Lock()
	paused, share, total := m.f.paused, m.f.share, m.f.m.nblocks
	full := m.wantFull
	m.wantFull = false
	m.mu.Unlock()

	switch {
	case full:
		m.scanRange(0, total)
		m.cursor = 0
		m.collect()
		m.mu.Lock()
		m.f.scanned = 1
		m.ready = true
		m.mu.Unlock()

	case !paused:
		chunk := total / share
		if chunk < 1 {
			chunk = 1
		}
		m.scanRange(m.cursor, m.cursor+chunk)
		m.cursor += chunk
		if m.cursor >= total {
			m.cursor = 0
		}
		m.collect()
	}
	return scanDoneMsg{}
}

func (m *model) Init() tea.Cmd {
	if m.f.snapshot != "" {
		return nil
	}
	m.wantFull = true
	return m.scan
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.rows, m.cols = msg.Height, msg.Width
		return m, nil

	// The detail pane follows the pointer at once because it is text and costs
	// nothing. The box drawn into the picture waits for the pointer to stop, so
	// that sweeping across the map does not queue a repaint per cell.
	case tea.MouseMsg:
		// The wheel arrives as a mouse event like any other, so without this it
		// is silently filed as pointer motion and nothing scrolls.
		m.mu.Lock()
		switch msg.Button {
		case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
			m.f.wheel(msg.Button == tea.MouseButtonWheelDown, msg.Ctrl)
		default:
			if m.f.hoverCol == msg.X && m.f.hoverRow == msg.Y {
				m.mu.Unlock()
				return m, nil
			}
			m.f.hoverCol, m.f.hoverRow = msg.X, msg.Y
		}
		m.hoverSeq++
		seq := m.hoverSeq
		m.mu.Unlock()

		return m, tea.Batch(catchUp(), tea.Tick(90*time.Millisecond, func(time.Time) tea.Msg {
			return hoverSettleMsg(seq)
		}))

	case hoverSettleMsg:
		m.mu.Lock()
		if uint64(msg) == m.hoverSeq {
			m.f.boxFrom, m.f.boxTo = m.f.hoverFrom, m.f.hoverTo
		}
		m.mu.Unlock()
		return m, nil

	case catchUpMsg:
		return m, nil

	case scanDoneMsg:
		return m, tea.Tick(m.interval, func(time.Time) tea.Msg { return m.scan() })

	case tea.KeyMsg:
		m.mu.Lock()
		quit, compact := false, false

		// Runes arriving in one read come as a single message, so holding a
		// key down hands us "---" rather than three separate "-".
		keys := []string{msg.String()}
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 {
			keys = keys[:0]
			for _, r := range msg.Runes {
				keys = append(keys, string(r))
			}
		}
		for _, k := range keys {
			q, c := m.key(k)
			quit = quit || q
			compact = compact || c
		}
		if compact {
			// Ask the running scan chain for a full sweep rather than
			// starting one here, which would fork a second scanner.
			m.wantFull = true
		}
		m.mu.Unlock()

		if quit {
			return m, tea.Quit
		}
		return m, catchUp()
	}
	return m, nil
}

// key applies one keystroke, reporting whether to quit and whether a fresh
// sweep is needed. The caller holds the lock.
func (m *model) key(k string) (quit, compact bool) {
	f := m.f
	span := f.viewTo - f.viewFrom

	// One map row is the natural scrolling step, but its size is only known
	// once something has been drawn.
	row := f.rowBlocks
	if row < 1 {
		row = 512
	}
	screen := f.screenBlocks
	if screen < 1 {
		screen = row * 8
	}

	// Scrolling means the map in one mode and the cache list in the other,
	// so the same keys are routed by what is on screen.
	if f.focusSlab {
		switch k {
		case "down", "j":
			f.slabScroll++
		case "up", "k":
			f.slabScroll--
		case "pgdown", "ctrl+d":
			f.slabScroll += f.slabRows
		case "pgup", "ctrl+u":
			f.slabScroll -= f.slabRows
		case "home", "g":
			f.slabScroll = 0
		case "end", "G":
			f.slabScroll = f.slabTotal
		}
	}

	switch k {
	case "q", "ctrl+c", "esc":
		return true, false
	case " ":
		f.paused = !f.paused
	case "tab", "s":
		f.focusSlab = !f.focusSlab
	case "o":
		f.slabSort = (f.slabSort + 1) % len(slabSorts)
		f.slabScroll = 0
	case "m":
		f.byClass = !f.byClass
	case "p":
		f.pixels = !f.pixels && kittyAvailable()
	case "c":
		os.WriteFile("/proc/sys/vm/compact_memory", []byte("1"), 0)
		return false, true
	}
	if f.focusSlab {
		return false, false
	}

	switch k {
	case "z":
		f.zoom(span / 2)
	case "Z":
		f.zoom(span * 2)
	case "down", "j":
		f.pan(row)
	case "up", "k":
		f.pan(-row)
	case "pgdown", "ctrl+d":
		f.pan(screen)
	case "pgup", "ctrl+u":
		f.pan(-screen)
	case "right", "l":
		f.pan(max(row/4, 1))
	case "left", "h":
		f.pan(-max(row/4, 1))
	case "home", "g":
		f.viewFrom, f.viewTo = 0, f.m.nblocks
		f.clamp()
	case "end", "G":
		f.viewFrom, f.viewTo = f.m.nblocks-span, f.m.nblocks
		f.clamp()
	// Doubling rather than stepping, so that one press is unmistakable.
	case "+", "=":
		if f.share > 1 {
			f.share /= 2
		}
	case "-":
		if f.share < 256 {
			f.share *= 2
		}
	}
	return false, false
}

func (m *model) View() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	rows, cols := m.rows, m.cols
	if rows == 0 || cols == 0 {
		// No size reported yet, or the terminal never told us. Draw at a
		// usable default rather than sitting on a placeholder forever.
		rows, cols = termSizeFallback()
		if rows == 0 {
			rows, cols = 40, 120
		}
	}
	if !m.ready {
		return "reading /proc/kpageflags…"
	}
	return m.f.render(rows, cols)
}

func main() {
	interval := flag.Duration("i", 500*time.Millisecond, "redraw interval")
	share := flag.Int("s", 8, "frames needed to refresh the whole map")
	once := flag.Bool("1", false, "print a single frame and exit")
	slab := flag.Bool("slab", false, "start on the full slab cache table")
	order := flag.Int("sort", 0, "slab sort order: 0 hostage, 1 blocks, 2 least dense, 3 size")
	flags := flag.Bool("flags", false, "histogram of raw page flags and exit")
	hostage := flag.Bool("hostage", false, "what pins the unusable blocks, then exit")
	dump := flag.String("dump", "", "write a snapshot of the current state and exit")
	load := flag.String("load", "", "render a snapshot instead of this machine")
	label := flag.String("label", "", "name for the snapshot being written")
	stamp := flag.String("stamp", "", "time to record in the snapshot")
	cmp := flag.Bool("cmp", false, "compare the snapshots named on the command line")
	root := flag.String("proc-root", "/proc", "read kernel reporting from here instead")
	colOrder := flag.String("order", "", "with -cmp, comma separated column order")
	title := flag.String("title", "", "with -cmp, heading for the markdown output")
	txtOut := flag.String("txt", "", "with -cmp, also write the table here")
	mdOut := flag.String("md", "", "with -cmp, also write markdown here")
	jsonOut := flag.String("json", "", "with -cmp, also write metrics here")
	var expect stringList
	flag.Var(&expect, "expect", "with -cmp, an assertion like pinned:stock->separation<=0.8")
	flag.Parse()
	procRoot = *root

	if *cmp {
		if flag.NArg() == 0 {
			fmt.Fprintln(os.Stderr, "fragview: -cmp wants a list of snapshot files")
			os.Exit(1)
		}
		var cols []string
		if *colOrder != "" {
			cols = strings.Split(*colOrder, ",")
		}
		compareSnapshots(flag.Args(), compareOpts{
			order:   cols,
			expect:  expect,
			txtOut:  *txtOut,
			mdOut:   *mdOut,
			jsonOut: *jsonOut,
			title:   *title,
		})
		return
	}

	// A snapshot brings its own machine with it, so none of the sizing below
	// applies and nothing may touch /proc.
	if *load != "" {
		f, err := readSnapshot(*load)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fragview:", err)
			os.Exit(1)
		}
		f.share, f.interval = *share, *interval
		f.focusSlab, f.slabSort = *slab, *order%len(slabSorts)
		f.pixels = kittyAvailable()
		if *hostage {
			hostageReport(f)
			return
		}
		showFrame(&model{f: f, ppb: f.pagesPerBlock, interval: *interval, ready: true}, *once)
		return
	}

	ppb := uint64(1) << readPageblockOrder()

	bm, err := newBlockMap(ppb)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fragview:", err)
		os.Exit(1)
	}

	// The window has to cover the whole map from the start: a key pressed
	// during the opening sweep would otherwise be handed a zero-width view
	// and zoom straight to the minimum.
	m := &model{
		f: &frame{m: bm, pagesPerBlock: ppb, host: hostname(),
			share: *share, interval: *interval, viewTo: bm.nblocks,
			focusSlab: *slab, slabSort: *order % len(slabSorts),
			pixels: kittyAvailable(), hoverCol: -1, hoverRow: -1},
		ppb:      ppb,
		interval: *interval,
	}

	nameClassesForHost(len(readARC()) > 0)
	abdMovable = abdRelocationBuilt()

	if *flags {
		dumpFlags(bm, ppb, 40)
		return
	}

	if *hostage {
		m.wantFull = true
		m.scan()
		m.f.sites = readSites()
		hostageReport(m.f)
		return
	}

	if *dump != "" {
		m.wantFull = true
		m.scan()
		m.f.sites = readSites()
		m.f.taken = *stamp
		m.f.summarise()
		if err := writeSnapshot(m.f, *dump, *label); err != nil {
			fmt.Fprintln(os.Stderr, "fragview:", err)
			os.Exit(1)
		}
		fmt.Printf("%s: %d blocks, %d hostage, pinning %s\n", *dump,
			m.f.m.nblocks, m.f.hostages, humanBytes(m.f.hostageFree*pageSize))
		return
	}

	if *once {
		m.wantFull = true
		m.scan()
		m.ready = true
	}
	showFrame(m, *once)
}

// Either one frame on stdout or the whole program, depending on which the
// caller asked for. Shared so that a snapshot behaves exactly like a machine.
func showFrame(m *model, once bool) {
	if once {
		m.rows, m.cols = 50, 200
		if r, c := termSizeFallback(); r > 0 {
			m.rows, m.cols = r, c
		}
		fmt.Println(m.f.render(m.rows, m.cols))
		return
	}

	if _, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseAllMotion()).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "fragview:", err)
		os.Exit(1)
	}
}

// Only used by the single-frame mode, where there may be no terminal at all.
func termSizeFallback() (int, int) {
	r, _ := strconv.Atoi(os.Getenv("LINES"))
	c, _ := strconv.Atoi(os.Getenv("COLUMNS"))
	if r > 0 && c > 0 {
		return r, c
	}
	return 0, 0
}
