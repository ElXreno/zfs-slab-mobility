package main

import (
	"fmt"
	"sort"
)

// What is actually pinning the blocks that cannot be handed out.
//
// /proc/slabwho answers this for slab pages and finds that most hostage blocks
// hold something it cannot name. This puts a name on the rest, which decides
// whether per-cache relocation work is worth doing at all.
func hostageReport(f *frame) {
	f.summarise()

	var byClass [nrClasses]uint64
	var soleClass [nrClasses]uint64
	var freeIn uint64
	blocks, mixed := 0, 0
	kinds := map[int]int{}

	for i, c := range f.m.counts {
		if !f.views[i].hostage {
			continue
		}
		blocks++
		freeIn += uint64(c[clFree])

		n, only := 0, clNone
		for k := clFree + 1; k < nrClasses; k++ {
			if !k.immovable() || c[k] == 0 {
				continue
			}
			byClass[k] += uint64(c[k])
			n++
			only = k
		}
		kinds[n]++
		if n == 1 {
			soleClass[only]++
		} else if n > 1 {
			mixed++
		}
	}

	fmt.Printf("%d hostage blocks, %s free inside them\n\n", blocks, humanBytes(freeIn*pageSize))
	fmt.Printf("%-12s %12s %14s %s\n", "class", "pages", "bytes", "blocks where it is alone")
	type row struct {
		c     class
		pages uint64
	}
	var rows []row
	for k := clFree + 1; k < nrClasses; k++ {
		if byClass[k] > 0 {
			rows = append(rows, row{k, byClass[k]})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].pages > rows[j].pages })
	for _, r := range rows {
		fmt.Printf("%-12s %12d %14s %19d\n", classNames[r.c], r.pages,
			humanBytes(r.pages*pageSize), soleClass[r.c])
	}

	// What each line of attack could reach on its own, and together. A block
	// only becomes free when every immovable class in it can be moved out.
	fmt.Printf("\nceiling by owner set (blocks whose only immovable pages are these):\n")
	for _, set := range [][]class{
		{clSlab},
		{clABD},
		{clSlab, clABD},
		{clSlab, clABD, clKernel},
	} {
		n, freed := 0, uint64(0)
		for i, c := range f.m.counts {
			if !f.views[i].hostage {
				continue
			}
			ok := true
			for k := clFree + 1; k < nrClasses; k++ {
				if !k.immovable() || c[k] == 0 {
					continue
				}
				in := false
				for _, s := range set {
					if s == k {
						in = true
					}
				}
				if !in {
					ok = false
					break
				}
			}
			if ok {
				n++
				freed += uint64(c[clFree])
			}
		}
		names := ""
		for i, s := range set {
			if i > 0 {
				names += "+"
			}
			names += classNames[s]
		}
		fmt.Printf("  %-22s %5d blocks, would return %s\n", names, n, humanBytes(freed*pageSize))
	}

	fmt.Printf("\ndistinct immovable classes per block:\n")
	var ks []int
	for k := range kinds {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	for _, k := range ks {
		fmt.Printf("  %d: %d blocks\n", k, kinds[k])
	}
	fmt.Printf("\nmixed (more than one class): %d of %d\n", mixed, blocks)
	hostageByCache(f, 12)
}

// Which caches own the slab pages left inside hostage blocks. Answering it from
// a snapshot rather than a live machine is the only way to compare two builds:
// the interesting state lasts seconds and exists inside a test VM.
func hostageByCache(f *frame, top int) {
	if f.who == nil {
		fmt.Println("\nno per-cache data in this snapshot")
		return
	}

	type row struct {
		name   string
		blocks uint64
		pages  uint64
	}
	var rows []row
	for name, w := range f.who {
		if w.hostageBlocks == 0 {
			continue
		}
		rows = append(rows, row{name, w.hostageBlocks, w.hostagePages})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].pages > rows[j].pages })

	fmt.Printf("\n%-28s %10s %12s\n", "cache", "blocks", "pages")
	var shown []string
	for i, r := range rows {
		if i >= top {
			break
		}
		shown = append(shown, r.name)
		fmt.Printf("%-28s %10d %12d\n", trunc(cacheLabel(r.name), 28), r.blocks, r.pages)
	}
	fmt.Print(aliasLegend(shown))
	hostageBySite(f, 15)
}

// The line of code that allocated what is sitting in the hostage blocks. A
// merged cache cannot answer this and neither can /proc/slabinfo: it needs the
// per object codetag, which exists only on a kernel built with allocation
// profiling.
func hostageBySite(f *frame, top int) {
	if len(f.sites) == 0 {
		return
	}

	fmt.Printf("\n%-22s %-26s %-22s %7s %8s\n",
		"cache", "allocated at", "in", "blocks", "objects")
	for i, si := range f.sites {
		if i >= top {
			break
		}
		fmt.Printf("%-22s %-26s %-22s %7d %8d\n", trunc(cacheLabel(si.cache), 22),
			trunc(si.fn, 26), trunc(si.file, 22), si.blocks, si.objects)
	}
}
