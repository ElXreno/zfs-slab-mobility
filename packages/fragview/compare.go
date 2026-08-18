// Putting snapshots side by side and deciding whether the difference holds up.
//
// Which number to lead with depends on the machine. Hostage blocks read as the
// headline and swing by a fifth between seeds. Blocks with more than one
// immovable owner are steadier, but once the ARC holds most of memory almost
// every block has an ARC page and the count stops distinguishing anything. What
// survives both is narrower: the slab pages left inside blocks that are
// otherwise nearly empty.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type snapMetrics struct {
	label    string
	kernel   string
	nblocks  int
	hostage  int
	pinned   uint64
	mixed    int
	slabIn   uint64
	arcIn    uint64
	order10  uint64
	unusable float64
	types    map[string]uint64
	arcSize  uint64
	slabAll  uint64
	kswapd   uint64
	direct   uint64
	huge     uint64
}

func measure(f *frame) snapMetrics {
	f.summarise()

	m := snapMetrics{
		label:   f.snapshot,
		kernel:  f.kernel,
		nblocks: f.m.nblocks,
		hostage: f.hostages,
		pinned:  f.hostageFree * pageSize,
		types:   map[string]uint64{},
		arcSize: f.arc["size"],
		slabAll: f.mem["Slab"],
		kswapd:  f.vm["pgscan_kswapd"],
		direct:  f.vm["pgscan_direct"],
		huge:    f.mem["HugePages_Total"],
	}

	for i, c := range f.m.counts {
		owners := 0
		for cl := clFree + 1; cl < nrClasses; cl++ {
			if cl.immovable() && c[cl] > 0 {
				owners++
			}
		}
		if owners > 1 {
			m.mixed++
		}
		if i < len(f.views) && f.views[i].hostage {
			m.slabIn += uint64(c[clSlab])
			m.arcIn += uint64(c[clABD])
		}
	}

	for i, n := range f.types.counts {
		if i < len(f.types.names) {
			m.types[f.types.names[i]] = n
		}
	}
	if o := len(f.ladder.orders) - 1; o >= 0 {
		m.order10 = f.ladder.orders[o]
		if ui := f.ladder.unusableIndex(); ui != nil {
			m.unusable = ui[o] * 100
		}
	}
	return m
}

func (m snapMetrics) fields() map[string]float64 {
	out := map[string]float64{
		"mixed":       float64(m.mixed),
		"hostage":     float64(m.hostage),
		"pinned":      float64(m.pinned),
		"slab_in":     float64(m.slabIn),
		"arc_in":      float64(m.arcIn),
		"order10":     float64(m.order10),
		"unusable":    m.unusable,
		"arc_size":    float64(m.arcSize),
		"slab_size":   float64(m.slabAll),
		"kswapd_scan": float64(m.kswapd),
		"hugepages":   float64(m.huge),
		"direct_scan": float64(m.direct),
		"blocks":      float64(m.nblocks),
	}
	for name, n := range m.types {
		out["blocks_"+strings.ToLower(name)] = float64(n)
	}
	return out
}

// Rows of the table, in the order they are printed. Leading with mixed blocks
// is deliberate: see the note at the top of the file.
var metricRows = []struct {
	key, title string
	bytes      bool
	percent    bool
	gapAfter   bool
}{
	{key: "mixed", title: "mixed blocks"},
	{key: "hostage", title: "hostage blocks"},
	{key: "pinned", title: "pinned", bytes: true},
	{key: "slab_in", title: "slab pages in them"},
	{key: "arc_in", title: "arc pages in them", gapAfter: true},
	{key: "hugepages", title: "huge pages handed out"},
	{key: "order10", title: "free at order 10"},
	{key: "unusable", title: "unusable order-10", percent: true, gapAfter: true},
	{key: "blocks_unmovable", title: "Unmovable blocks"},
	{key: "blocks_movable", title: "Movable blocks"},
	{key: "blocks_reclaimable", title: "Reclaimable blocks", gapAfter: true},
	{key: "kswapd_scan", title: "pages scanned by kswapd"},
	{key: "direct_scan", title: "pages scanned directly", gapAfter: true},
	{key: "arc_size", title: "ARC", bytes: true},
	{key: "slab_size", title: "Slab", bytes: true},
	{key: "blocks", title: "blocks total"},
}

func showValue(key string, v float64) string {
	for _, r := range metricRows {
		if r.key != key {
			continue
		}
		if r.bytes {
			return humanBytes(uint64(v))
		}
		if r.percent {
			return fmt.Sprintf("%.0f%%", v)
		}
	}
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return fmt.Sprintf("%.3f", v)
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// A build's numbers, one entry per seed, reduced to a median.
type group struct {
	label  string
	seeds  []map[string]float64
	median map[string]float64
}

// The label a snapshot carries is "<build>/<phase>"; the build is what groups
// them, so which files belong together does not have to be passed in.
func groupSnapshots(paths []string, order []string) ([]*group, error) {
	byLabel := map[string]*group{}
	var seen []string

	for _, p := range paths {
		f, err := readSnapshot(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		name, _, _ := strings.Cut(f.snapshot, "/")
		g := byLabel[name]
		if g == nil {
			g = &group{label: name}
			byLabel[name] = g
			seen = append(seen, name)
		}
		g.seeds = append(g.seeds, measure(f).fields())
	}

	for _, g := range byLabel {
		g.median = map[string]float64{}
		for key := range g.seeds[0] {
			xs := make([]float64, 0, len(g.seeds))
			for _, s := range g.seeds {
				xs = append(xs, s[key])
			}
			g.median[key] = median(xs)
		}
	}

	if len(order) == 0 {
		order = seen
	}
	out := make([]*group, 0, len(order))
	for _, name := range order {
		if g := byLabel[name]; g != nil {
			out = append(out, g)
		}
	}
	for _, name := range seen {
		if !contains(order, name) {
			out = append(out, byLabel[name])
		}
	}
	return out, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Every column after the first carries its change against that first column.
// Absolute counts depend on the guest and the workload; the change between two
// builds measured back to back is the part that means anything.
func cell(key string, gs []*group, i int) string {
	v := gs[i].median[key]
	text := showValue(key, v)
	if i == 0 {
		return text
	}
	base := gs[0].median[key]
	if base == 0 {
		return text
	}
	return fmt.Sprintf("%s (%+.0f%%)", text, (v/base-1)*100)
}

func tableCells(gs []*group) ([][]string, int) {
	rows := make([][]string, len(metricRows))
	width := 0
	for ri, r := range metricRows {
		rows[ri] = make([]string, len(gs))
		for i := range gs {
			c := cell(r.key, gs, i)
			rows[ri][i] = c
			if n := utf8.RuneCountInString(c); n > width {
				width = n
			}
		}
	}
	for _, g := range gs {
		if n := utf8.RuneCountInString(g.label); n > width {
			width = n
		}
	}
	return rows, width + 2
}

func textTable(gs []*group) string {
	rows, col := tableCells(gs)

	var b strings.Builder
	head := fmt.Sprintf("%-26s", "")
	for _, g := range gs {
		head += fmt.Sprintf("%*s", col, g.label)
	}
	fmt.Fprintln(&b, head)
	fmt.Fprintln(&b, strings.Repeat("─", utf8.RuneCountInString(head)))

	for ri, r := range metricRows {
		line := fmt.Sprintf("%-26s", r.title)
		for i := range gs {
			line += fmt.Sprintf("%*s", col, rows[ri][i])
		}
		fmt.Fprintln(&b, line)
		if r.gapAfter {
			fmt.Fprintln(&b)
		}
	}
	return b.String()
}

func markdownTable(gs []*group) string {
	rows, _ := tableCells(gs)

	var b strings.Builder
	b.WriteString("|  |")
	for _, g := range gs {
		fmt.Fprintf(&b, " %s |", g.label)
	}
	b.WriteString("\n|---|")
	for range gs {
		b.WriteString("---:|")
	}
	b.WriteString("\n")

	for ri, r := range metricRows {
		fmt.Fprintf(&b, "| %s |", r.title)
		for i := range gs {
			fmt.Fprintf(&b, " %s |", rows[ri][i])
		}
		b.WriteString("\n")
	}
	return b.String()
}

// One assertion, written as metric:from->to<=ratio or >=ratio.
type expectation struct {
	metric, from, to string
	atMost           bool
	bound            float64
}

func parseExpectation(spec string) (expectation, error) {
	var e expectation
	metric, rest, ok := strings.Cut(spec, ":")
	if !ok {
		return e, fmt.Errorf("no metric in %q", spec)
	}
	e.metric = metric

	op := "<="
	if !strings.Contains(rest, "<=") {
		op = ">="
	}
	pair, boundStr, ok := strings.Cut(rest, op)
	if !ok {
		return e, fmt.Errorf("no <= or >= in %q", spec)
	}
	e.atMost = op == "<="

	from, to, ok := strings.Cut(pair, "->")
	if !ok {
		return e, fmt.Errorf("no -> in %q", spec)
	}
	e.from, e.to = from, to

	b, err := strconv.ParseFloat(boundStr, 64)
	if err != nil {
		return e, fmt.Errorf("bad bound in %q: %w", spec, err)
	}
	e.bound = b
	return e, nil
}

type verdict struct {
	expectation
	a, b, ratio float64
	ok          bool
	missing     string
	nosignal    bool
}

func judge(gs []*group, specs []expectation) []verdict {
	byLabel := map[string]*group{}
	for _, g := range gs {
		byLabel[g.label] = g
	}

	out := make([]verdict, 0, len(specs))
	for _, e := range specs {
		v := verdict{expectation: e}
		from, to := byLabel[e.from], byLabel[e.to]
		switch {
		case from == nil:
			v.missing = e.from
		case to == nil:
			v.missing = e.to
		default:
			v.a, v.b = from.median[e.metric], to.median[e.metric]
			if v.a == 0 {
				// Nothing to compare against. A ratio of zero would satisfy any
				// upper bound and the assertion would pass having measured
				// nothing, which is worse than no assertion at all.
				v.nosignal = true
				break
			}
			v.ratio = v.b / v.a
			v.ok = v.ratio <= e.bound
			if !e.atMost {
				v.ok = v.ratio >= e.bound
			}
		}
		out = append(out, v)
	}
	return out
}

func (v verdict) op() string {
	if v.atMost {
		return "<="
	}
	return ">="
}

func (v verdict) state() string {
	if v.missing != "" {
		return "MISSING " + v.missing
	}
	if v.nosignal {
		return "NO SIGNAL"
	}
	if v.ok {
		return "ok"
	}
	return "FAILED"
}

func verdictText(vs []verdict) string {
	var b strings.Builder
	for _, v := range vs {
		fmt.Fprintf(&b, "%-12s %-12s %10s -> %-12s %10s  ratio %.3f  %s (needs %s %g)\n",
			v.metric, v.from, showValue(v.metric, v.a), v.to, showValue(v.metric, v.b),
			v.ratio, v.state(), v.op(), v.bound)
	}
	return b.String()
}

func verdictMarkdown(vs []verdict) string {
	var b strings.Builder
	b.WriteString("| metric | from | to | ratio | needs | |\n")
	b.WriteString("|---|---|---|---:|---:|---|\n")
	for _, v := range vs {
		mark := "✅"
		if v.nosignal {
			mark = "⚠️"
		} else if !v.ok {
			mark = "❌"
		}
		fmt.Fprintf(&b, "| %s | %s %s | %s %s | %.3f | %s %g | %s |\n",
			v.metric, v.from, showValue(v.metric, v.a), v.to, showValue(v.metric, v.b),
			v.ratio, v.op(), v.bound, mark)
	}
	return b.String()
}

func writeFile(path, content string) {
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fragview:", err)
		os.Exit(1)
	}
}

type compareOpts struct {
	order          []string
	expect         []string
	txtOut, mdOut  string
	jsonOut, title string
}

func compareSnapshots(paths []string, o compareOpts) {
	gs, err := groupSnapshots(paths, o.order)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fragview:", err)
		os.Exit(1)
	}

	var specs []expectation
	for _, s := range o.expect {
		e, err := parseExpectation(s)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fragview:", err)
			os.Exit(1)
		}
		specs = append(specs, e)
	}
	vs := judge(gs, specs)

	text := textTable(gs)
	fmt.Print(text)
	if len(vs) > 0 {
		fmt.Print("\n", verdictText(vs))
	}

	writeFile(o.txtOut, text+"\n"+verdictText(vs))

	if o.mdOut != "" {
		var b strings.Builder
		if o.title != "" {
			fmt.Fprintf(&b, "## %s\n\n", o.title)
		}
		if len(vs) > 0 {
			b.WriteString(verdictMarkdown(vs))
			b.WriteString("\n")
		}
		b.WriteString("<details><summary>every metric, median of ")
		fmt.Fprintf(&b, "%d seeds</summary>\n\n", len(gs[0].seeds))
		b.WriteString(markdownTable(gs))
		b.WriteString("\n</details>\n")
		writeFile(o.mdOut, b.String())
	}

	if o.jsonOut != "" {
		payload := map[string]any{}
		for _, g := range gs {
			payload[g.label] = map[string]any{"median": g.median, "seeds": g.seeds}
		}
		out, _ := json.MarshalIndent(payload, "", "  ")
		writeFile(o.jsonOut, string(out)+"\n")
	}

	for _, v := range vs {
		if !v.ok {
			os.Exit(1)
		}
	}
}
