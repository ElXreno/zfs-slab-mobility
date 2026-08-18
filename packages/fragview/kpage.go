// Reading the kernel's per-page view of physical memory.
//
// /proc/kpageflags is eight bytes of flags for every page frame in the machine.
// On a 62G box that is sixteen million entries, so a full sweep costs about a
// second of CPU and has to be spread over several frames rather than done at
// once. The file supports seeking, which is what makes that possible.
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	kpfLocked = iota
	kpfError
	kpfReferenced
	kpfUptodate
	kpfDirty
	kpfLRU
	kpfActive
	kpfSlab
	kpfWriteback
	kpfReclaim
	kpfBuddy
	kpfMmap
	kpfAnon
	kpfSwapcache
	kpfSwapbacked
	kpfCompoundHead
	kpfCompoundTail
	kpfHuge
	kpfUnevictable
	kpfHWPoison
	kpfNoPage
	kpfKSM
	kpfTHP
	kpfOffline
	kpfZeroPage
	kpfIdle
	kpfPgTable
)

// Bits 32 and up are the kernel's own bookkeeping, exported at the end of the
// word. KPF_RESERVED is the interesting one here: it covers the kernel image,
// the memory map itself and firmware reservations, none of which ever move.
const (
	kpfReserved = 32 + iota
	kpfMlocked
	kpfOwner2 // PG_owner_2; on anonymous folios this is anon_exclusive
	kpfPrivate
	kpfPrivate2
	kpfOwnerPrivate // PG_owner_priv_1; on anonymous folios this is swapcache
	kpfArch
	_
	kpfSoftDirty
	kpfArch2
	kpfArch3
)

// What a page is being used for, in the only terms that matter for
// fragmentation: can this page be moved out of the way, or not.
type class uint8

const (
	clNone class = iota // outside RAM, or a hole
	clFree
	clFile     // page cache, movable and reclaimable
	clShmem    // tmpfs and other swap-backed page cache, movable
	clAnon     // anonymous, movable
	clABD      // ZFS ARC data, immovable
	clSlab     // slab, immovable without a relocation callback
	clPgtab    // page tables, immovable
	clCompound // some other high order allocation straight from the page allocator
	clUncached // taken out of write-back caching for a device, almost all GPU
	clKernel   // no flags at all: vmalloc, driver buffers, per-cpu
	clReserved // kernel image, memory map, firmware
	clUnknown  // has flags, matched no rule: a gap in the classifier
	nrClasses
)

var classNames = [nrClasses]string{"absent", "free", "cache", "shmem", "anon",
	"arc", "slab", "pgtable", "compound", "uncached", "kernel", "reserved", "unknown"}

// PG_private off the LRU and out of slab is ZFS ARC data on the machines this
// was written for, and on a machine without ZFS it is something else entirely.
// Rather than label another filesystem's pages as ARC, drop back to naming the
// flag that was actually seen.
func nameClassesForHost(haveZFS bool) {
	if !haveZFS {
		classNames[clABD] = "private"
	}
}

// Set when the machine the numbers came from carries the patch that lets the
// allocator relocate ARC chunks. Package level for the same reason the names
// above are: a class knows nothing about the machine it was read from, and
// judging movability by a flag is not possible here because the page type the
// patch uses is not among the ones kpageflags exports.
var abdMovable bool

// True for classes the page allocator cannot migrate out of a pageblock. This
// is the whole reason the tool exists: one such page in an otherwise empty
// block is enough to deny an order-9 allocation.
func (c class) immovable() bool {
	switch c {
	case clABD:
		return !abdMovable
	case clSlab, clPgtab, clCompound, clUncached, clKernel, clReserved, clUnknown:
		return true
	}
	return false
}

func classify(f uint64) class {
	bit := func(b uint) bool { return f&(1<<b) != 0 }

	switch {
	case bit(kpfNoPage), bit(kpfOffline):
		return clNone
	case bit(kpfBuddy):
		return clFree
	case bit(kpfSlab):
		return clSlab
	case bit(kpfPgTable):
		return clPgtab
	case bit(kpfReserved):
		return clReserved
	case bit(kpfLRU):
		// Swap-backed page cache without the anon bit is tmpfs. It is movable
		// like anything else on the LRU, but it is not process memory and it
		// is not dropped under pressure, which is worth seeing separately.
		if bit(kpfAnon) || bit(kpfKSM) {
			return clAnon
		}
		if bit(kpfSwapbacked) {
			return clShmem
		}
		return clFile
	case bit(kpfPrivate):
		// abd_mark_zfs_page() in module/os/linux/zfs/abd_os.c takes a
		// reference and sets PG_private on every scatter ABD page, head and
		// tail alike. Off the LRU and out of slab, that combination is ARC
		// data and nothing else on these machines.
		return clABD
	case bit(kpfCompoundHead), bit(kpfCompoundTail):
		return clCompound
	case bit(kpfArch), bit(kpfArch2):
		// On x86 the two arch bits together are the PAT memory type of the page:
		// arch1 alone is write-combining, arch2 alone uncached-minus, both
		// write-through (_PGMT_* in arch/x86/mm/pat/memtype.c). A page leaves the
		// default write-back type when a driver maps it for a device, so this is
		// the GPU's memory and it does not move. On the machine this was written
		// for it came to 1.75 GiB, all of it in the unnamed pile until now.
		return clUncached
	case f == 0:
		return clKernel
	}

	// Every case above is a rule about what a page is for. Reaching here means
	// the page carries flags none of them accounted for, which is a gap in this
	// function rather than a kind of memory. The scan records the combination so
	// the gap can be closed.
	return clUnknown
}

// Flag combinations that fell through classify(), most common first. The scan
// collects them locally and merges once per pass, so the hot loop stays out of
// the lock even when a whole zone turns out to be unrecognised.
var (
	unknownMu    sync.Mutex
	unknownFlags = map[uint64]uint64{}
)

func mergeUnknown(seen map[uint64]uint64) {
	unknownMu.Lock()
	defer unknownMu.Unlock()

	for f, n := range seen {
		if _, ok := unknownFlags[f]; !ok && len(unknownFlags) >= 256 {
			continue
		}
		unknownFlags[f] += n
	}
}

type unknownCombo struct {
	flags uint64
	count uint64
}

func unknownSeen() []unknownCombo {
	unknownMu.Lock()
	out := make([]unknownCombo, 0, len(unknownFlags))
	for f, n := range unknownFlags {
		out = append(out, unknownCombo{f, n})
	}
	unknownMu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].count > out[j].count })
	return out
}

// A contiguous run of page frames that is actually RAM. Everything between
// these is firmware, MMIO or holes, and reading flags there is pointless.
type ramRange struct{ first, last uint64 }

func readRAMRanges() ([]ramRange, error) {
	f, err := openProc("iomem")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []ramRange
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasSuffix(strings.TrimSpace(line), "System RAM") {
			continue
		}
		// Nested entries are indented and live inside a range we already have.
		if strings.HasPrefix(line, " ") {
			continue
		}
		parts := strings.SplitN(strings.TrimSpace(line), " :", 2)
		bounds := strings.SplitN(parts[0], "-", 2)
		if len(bounds) != 2 {
			continue
		}
		lo, err1 := strconv.ParseUint(bounds[0], 16, 64)
		hi, err2 := strconv.ParseUint(bounds[1], 16, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, ramRange{first: lo / pageSize, last: hi / pageSize})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no System RAM line in /proc/iomem (needs root)")
	}
	return out, sc.Err()
}

// The map itself: one counter per class for every pageblock.
type blockMap struct {
	firstPFN uint64
	nblocks  int
	counts   [][nrClasses]uint16

	kpage  flagSource
	ranges []ramRange
}

func newBlockMap(pagesPerBlock uint64) (*blockMap, error) {
	ranges, err := readRAMRanges()
	if err != nil {
		return nil, err
	}
	f, err := openPageFile("kpageflags")
	if err != nil {
		return nil, fmt.Errorf("%w (needs root)", err)
	}

	first := ranges[0].first
	last := ranges[len(ranges)-1].last
	nb := int((last-first)/pagesPerBlock) + 1

	return &blockMap{
		firstPFN: first / pagesPerBlock * pagesPerBlock,
		nblocks:  nb,
		counts:   make([][nrClasses]uint16, nb),
		kpage:    f,
		ranges:   ranges,
	}, nil
}

func (m *blockMap) inRAM(pfn uint64) bool {
	for _, r := range m.ranges {
		if pfn >= r.first && pfn <= r.last {
			return true
		}
	}
	return false
}

// Refresh the blocks in [from, to) into dst, which is indexed from zero at
// from. Writing somewhere other than the live map is what lets the scan run
// without holding the lock the renderer needs; the caller swaps the result in
// afterwards. Reading is done in big chunks because the per-page cost in the
// kernel is small but the syscall cost is not.
//
// Both scratch areas belong to the caller rather than to the map, so two
// scans running at once can never end up reading into each other's buffer and
// painting one part of memory with pages from another.
func (m *blockMap) refreshInto(from, to int, pagesPerBlock uint64, dst [][nrClasses]uint16, buf []byte) error {
	if from < 0 {
		from = 0
	}
	if to > m.nblocks {
		to = m.nblocks
	}
	if to > from+len(dst) {
		to = from + len(dst)
	}
	perRead := uint64(len(buf) / 8)

	var unknown map[uint64]uint64
	defer func() {
		if unknown != nil {
			mergeUnknown(unknown)
		}
	}()

	for blk := from; blk < to; {
		startPFN := m.firstPFN + uint64(blk)*pagesPerBlock
		if !m.inRAM(startPFN) {
			dst[blk-from] = [nrClasses]uint16{}
			blk++
			continue
		}

		want := perRead / pagesPerBlock
		if want == 0 {
			want = 1
		}
		if int(want) > to-blk {
			want = uint64(to - blk)
		}
		pages := want * pagesPerBlock

		n, err := m.kpage.ReadAt(buf[:pages*8], int64(startPFN*8))
		if err != nil && n == 0 {
			return err
		}
		got := uint64(n / 8)

		for i := uint64(0); i < want; i++ {
			var c [nrClasses]uint16
			for j := uint64(0); j < pagesPerBlock; j++ {
				idx := i*pagesPerBlock + j
				if idx >= got {
					break
				}
				f := binary.LittleEndian.Uint64(buf[idx*8:])
				cl := classify(f)
				if cl == clUnknown {
					if unknown == nil {
						unknown = make(map[uint64]uint64)
					}
					unknown[f]++
				}
				c[cl]++
			}
			dst[blk+int(i)-from] = c
		}
		blk += int(want)
	}
	return nil
}
