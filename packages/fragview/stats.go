// The cheap counters: everything here is a few hundred bytes of /proc and can
// be re-read every frame without noticing the cost.
package main

import (
	"bufio"
	"sort"
	"strconv"
	"strings"
)

const pageSize = 4096

// Free pages per order, per zone, summed over nodes. Index is the order.
type freeLadder struct {
	orders   []uint64
	maxOrder int
}

func readBuddyinfo() (freeLadder, error) {
	f, err := openProc("buddyinfo")
	if err != nil {
		return freeLadder{}, err
	}
	defer f.Close()

	l := freeLadder{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// Node 0, zone   Normal  <counts...>
		idx := -1
		for i, s := range fields {
			if s == "zone" {
				idx = i + 2
				break
			}
		}
		if idx < 0 || idx > len(fields) {
			continue
		}
		for i, s := range fields[idx:] {
			n, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				continue
			}
			for len(l.orders) <= i {
				l.orders = append(l.orders, 0)
			}
			l.orders[i] += n
		}
	}
	l.maxOrder = len(l.orders) - 1
	return l, sc.Err()
}

// The fraction of free memory that cannot serve an allocation of each order.
// Zero means every free page is part of a block that big; one means none are.
func (l freeLadder) unusableIndex() []float64 {
	total := 0.0
	for o, n := range l.orders {
		total += float64(n) * float64(uint64(1)<<uint(o))
	}
	if total == 0 {
		return nil
	}
	out := make([]float64, len(l.orders))
	for want := range l.orders {
		lost := 0.0
		for o := 0; o < want; o++ {
			lost += float64(l.orders[o]) * float64(uint64(1)<<uint(o))
		}
		out[want] = lost / total
	}
	return out
}

// How many pageblocks the allocator has stamped with each migrate type. This
// is the "Number of blocks type" table at the bottom of /proc/pagetypeinfo, and
// it is the only place the kernel exports migrate types without page_owner.
type blockTypes struct {
	names  []string
	counts []uint64
}

func readPagetypeinfo() (blockTypes, error) {
	f, err := openProc("pagetypeinfo")
	if err != nil {
		return blockTypes{}, err
	}
	defer f.Close()

	bt := blockTypes{}
	inTable := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Number of blocks type") {
			inTable = true
			bt.names = strings.Fields(strings.TrimPrefix(line, "Number of blocks type"))
			bt.counts = make([]uint64, len(bt.names))
			continue
		}
		if !inTable || !strings.HasPrefix(line, "Node") {
			continue
		}
		fields := strings.Fields(line)
		// Node 0, zone   Normal <counts...>
		start := len(fields) - len(bt.names)
		if start < 0 {
			continue
		}
		for i, s := range fields[start:] {
			if n, err := strconv.ParseUint(s, 10, 64); err == nil {
				bt.counts[i] += n
			}
		}
	}
	return bt, sc.Err()
}

type slabCache struct {
	name         string
	active       uint64
	total        uint64
	objSize      uint64
	pagesPerSlab uint64
	slabs        uint64
	activeSlabs  uint64
}

func (c slabCache) bytes() uint64     { return c.total * c.objSize }
func (c slabCache) liveBytes() uint64 { return c.active * c.objSize }

// Memory sitting in allocated slabs with nothing in it. This is what a cache
// costs beyond what it is actually holding.
func (c slabCache) wasted() uint64 { return c.bytes() - c.liveBytes() }
func (c slabCache) occupancy() float64 {
	if c.total == 0 {
		return 0
	}
	return float64(c.active) / float64(c.total)
}

func readSlabinfo() ([]slabCache, error) {
	f, err := openProc("slabinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []slabCache
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "slabinfo") {
			continue
		}
		fl := strings.Fields(line)
		if len(fl) < 6 {
			continue
		}
		num := func(i int) uint64 {
			n, _ := strconv.ParseUint(fl[i], 10, 64)
			return n
		}
		c := slabCache{name: fl[0], active: num(1), total: num(2), objSize: num(3)}
		// <objperslab> <pagesperslab> then " : tunables ..." then " : slabdata <active_slabs> <num_slabs>"
		if len(fl) > 5 {
			c.pagesPerSlab = num(5)
		}
		for i, s := range fl {
			if s == "slabdata" && i+2 < len(fl) {
				c.activeSlabs = num(i + 1)
				c.slabs = num(i + 2)
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].bytes() > out[j].bytes() })
	return out, sc.Err()
}

func readMeminfo() map[string]uint64 {
	m := map[string]uint64{}
	f, err := openProc("meminfo")
	if err != nil {
		return m
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		m[k] = n * 1024
	}
	return m
}

func readVmstat() map[string]uint64 {
	m := map[string]uint64{}
	f, err := openProc("vmstat")
	if err != nil {
		return m
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		if n, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
			m[fields[0]] = n
		}
	}
	return m
}

// ARC is the elephant in the room on these machines and htop counts it as
// neither cache nor free, so it gets read separately.
func readARC() map[string]uint64 {
	m := map[string]uint64{}
	f, err := openProc("spl/kstat/zfs/arcstats")
	if err != nil {
		return m
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 3 {
			continue
		}
		if n, err := strconv.ParseUint(fields[2], 10, 64); err == nil {
			m[fields[0]] = n
		}
	}
	return m
}
