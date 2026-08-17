package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
)

var kpfNames = map[uint]string{
	kpfLocked: "locked", kpfError: "error", kpfReferenced: "referenced",
	kpfUptodate: "uptodate", kpfDirty: "dirty", kpfLRU: "lru", kpfActive: "active",
	kpfSlab: "slab", kpfWriteback: "writeback", kpfReclaim: "reclaim",
	kpfBuddy: "buddy", kpfMmap: "mmap", kpfAnon: "anon", kpfSwapcache: "swapcache",
	kpfSwapbacked: "swapbacked", kpfCompoundHead: "comp_head", kpfCompoundTail: "comp_tail",
	kpfHuge: "huge", kpfUnevictable: "unevictable", kpfHWPoison: "hwpoison",
	kpfNoPage: "nopage", kpfKSM: "ksm", kpfTHP: "thp", kpfOffline: "offline",
	kpfZeroPage: "zeropage", kpfIdle: "idle", kpfPgTable: "pgtable",
	kpfReserved: "reserved", kpfMlocked: "mlocked", kpfOwner2: "owner2/anon_excl",
	kpfPrivate: "private", kpfPrivate2: "private2", kpfOwnerPrivate: "ownerpriv/swapcache",
	kpfArch: "arch1", kpfSoftDirty: "softdirty", kpfArch2: "arch2", kpfArch3: "arch3",
}

func flagString(f uint64) string {
	s := ""
	for b := uint(0); b < 64; b++ {
		if f&(1<<b) == 0 {
			continue
		}
		if n, ok := kpfNames[b]; ok {
			s += n + ","
		} else {
			s += fmt.Sprintf("bit%d,", b)
		}
	}
	if s == "" {
		return "(none)"
	}
	return s[:len(s)-1]
}

type flagStat struct {
	pages   uint64
	refZero uint64
	refSum  uint64
}

// Histogram of raw flag words, so a classification gap shows up as a named
// combination instead of a pile in the catch-all class. /proc/kpagecount comes along for
// the ride: it reports how many address spaces map the page, which separates
// process memory from kernel allocations that merely look similar.
func dumpFlags(m *blockMap, ppb uint64, top int) {
	hist := map[uint64]*flagStat{}
	buf := make([]byte, 1<<20)
	cbuf := make([]byte, 1<<20)

	counts, err := openPageFile("kpagecount")
	if err != nil {
		counts = nil
	}

	for _, r := range m.ranges {
		for pfn := r.first; pfn <= r.last; {
			n := uint64(len(buf) / 8)
			if pfn+n > r.last+1 {
				n = r.last + 1 - pfn
			}
			got, err := m.kpage.ReadAt(buf[:n*8], int64(pfn*8))
			if err != nil && got == 0 {
				break
			}
			gotc := 0
			if counts != nil {
				gotc, _ = counts.ReadAt(cbuf[:n*8], int64(pfn*8))
			}

			for i := 0; i < got/8; i++ {
				f := binary.LittleEndian.Uint64(buf[i*8:])
				s := hist[f]
				if s == nil {
					s = &flagStat{}
					hist[f] = s
				}
				s.pages++
				if i*8 < gotc {
					c := binary.LittleEndian.Uint64(cbuf[i*8:])
					s.refSum += c
					if c == 0 {
						s.refZero++
					}
				}
			}
			pfn += n
		}
	}

	type ent struct {
		f uint64
		s *flagStat
	}
	var es []ent
	for f, s := range hist {
		es = append(es, ent{f, s})
	}
	sort.Slice(es, func(i, j int) bool { return es[i].s.pages > es[j].s.pages })

	fmt.Printf("%12s %8s %9s %8s  %-18s %s\n", "pages", "MiB", "unmapped", "avg maps", "class", "flags")
	for i := 0; i < top && i < len(es); i++ {
		s := es[i].s
		fmt.Printf("%12d %8d %8.0f%% %8.1f  %-18s %s\n",
			s.pages, s.pages>>8,
			float64(s.refZero)/float64(s.pages)*100,
			float64(s.refSum)/float64(s.pages),
			classNames[classify(es[i].f)], flagString(es[i].f))
	}
	os.Stdout.Sync()
}
