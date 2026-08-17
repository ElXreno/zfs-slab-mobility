// Freezing a machine's memory picture into a file, and looking at it later.
//
// The interesting states are the ones that only exist inside a test VM for a
// few seconds: ARC warmed, ARC squeezed, caches dropped. Reading them over a
// serial console is hopeless and screenshots cannot be compared. So the guest
// writes a snapshot per phase into a shared directory, and the same binary
// renders it on the host with every pane and key working as if it were live.
package main

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

const snapshotVersion = 1

type snapshot struct {
	Version int    `json:"version"`
	Label   string `json:"label"`
	Host    string `json:"host"`
	Kernel  string `json:"kernel"`
	Taken   string `json:"taken"`

	PagesPerBlock uint64 `json:"pages_per_block"`
	FirstPFN      uint64 `json:"first_pfn"`
	NBlocks       int    `json:"nblocks"`
	// Little endian uint16 per class per block. As JSON numbers this is two
	// megabytes of text for a 64G machine; as bytes it is under one.
	Counts string `json:"counts"`

	Ladder     []uint64 `json:"ladder"`
	MaxOrder   int      `json:"max_order"`
	TypeNames  []string `json:"type_names"`
	TypeCounts []uint64 `json:"type_counts"`

	Slabs  []snapSlab         `json:"slabs"`
	Who    map[string]snapWho `json:"who,omitempty"`
	WhoTot snapWhoTot         `json:"who_total"`
	WhoErr string             `json:"who_err,omitempty"`
	Mem    map[string]uint64  `json:"meminfo"`
	Vm     map[string]uint64  `json:"vmstat"`
	Arc    map[string]uint64  `json:"arcstats,omitempty"`
	Unseen map[string]uint64  `json:"unclassified,omitempty"`
}

type snapSlab struct {
	Name         string `json:"name"`
	Active       uint64 `json:"active"`
	Total        uint64 `json:"total"`
	ObjSize      uint64 `json:"objsize"`
	PagesPerSlab uint64 `json:"pages_per_slab"`
	Slabs        uint64 `json:"slabs"`
	ActiveSlabs  uint64 `json:"active_slabs"`
}

type snapWho struct {
	Blocks        uint64 `json:"blocks"`
	HostageBlocks uint64 `json:"hostage_blocks"`
	Pages         uint64 `json:"pages"`
	HostagePages  uint64 `json:"hostage_pages"`
	Mobile        bool   `json:"mobile"`
}

type snapWhoTot struct {
	Blocks           uint64 `json:"blocks"`
	HostageBlocks    uint64 `json:"hostage_blocks"`
	HostageFreePages uint64 `json:"hostage_free_pages"`
	WalkUS           uint64 `json:"walk_us"`
}

func kernelRelease() string {
	b, err := os.ReadFile(procPath("sys/kernel/osrelease"))
	if err != nil {
		return ""
	}
	return string(b[:len(b)-1])
}

func (f *frame) toSnapshot(label string) *snapshot {
	const stride = int(nrClasses)
	raw := make([]byte, len(f.m.counts)*stride*2)
	for i, c := range f.m.counts {
		for j, n := range c {
			binary.LittleEndian.PutUint16(raw[(i*stride+j)*2:], n)
		}
	}

	s := &snapshot{
		Version:       snapshotVersion,
		Label:         label,
		Host:          f.host,
		Kernel:        kernelRelease(),
		PagesPerBlock: f.pagesPerBlock,
		FirstPFN:      f.m.firstPFN,
		NBlocks:       f.m.nblocks,
		Counts:        base64.StdEncoding.EncodeToString(raw),
		Ladder:        f.ladder.orders,
		MaxOrder:      f.ladder.maxOrder,
		TypeNames:     f.types.names,
		TypeCounts:    f.types.counts,
		WhoErr:        f.whoErr,
		Mem:           f.mem,
		Vm:            f.vm,
		Arc:           f.arc,
		WhoTot: snapWhoTot{f.whoTot.blocks, f.whoTot.hostageBlocks,
			f.whoTot.hostageFreePages, f.whoTot.walkUS},
	}

	for _, c := range f.slabs {
		s.Slabs = append(s.Slabs, snapSlab{c.name, c.active, c.total,
			c.objSize, c.pagesPerSlab, c.slabs, c.activeSlabs})
	}
	if f.who != nil {
		s.Who = map[string]snapWho{}
		for k, w := range f.who {
			s.Who[k] = snapWho{w.blocks, w.hostageBlocks, w.pages, w.hostagePages, w.mobile}
		}
	}
	if seen := unknownSeen(); len(seen) > 0 {
		s.Unseen = map[string]uint64{}
		for _, u := range seen {
			s.Unseen[strconv.FormatUint(u.flags, 16)] = u.count
		}
	}
	return s
}

func writeSnapshot(f *frame, path, label string) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()

	zw := gzip.NewWriter(out)
	if err := json.NewEncoder(zw).Encode(f.toSnapshot(label)); err != nil {
		return err
	}
	return zw.Close()
}

// Rebuilds a frame that renders exactly like a live one, minus the parts that
// would want to read /proc: there is no kpageflags file behind it and nothing
// ever rescans.
func readSnapshot(path string) (*frame, error) {
	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer in.Close()

	zr, err := gzip.NewReader(in)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	var s snapshot
	if err := json.NewDecoder(zr).Decode(&s); err != nil {
		return nil, err
	}
	if s.Version != snapshotVersion {
		return nil, fmt.Errorf("snapshot is version %d, this build reads %d",
			s.Version, snapshotVersion)
	}

	raw, err := base64.StdEncoding.DecodeString(s.Counts)
	if err != nil {
		return nil, err
	}
	const stride = int(nrClasses)
	if len(raw) != s.NBlocks*stride*2 {
		return nil, fmt.Errorf("counters for %d blocks, header claims %d",
			len(raw)/(stride*2), s.NBlocks)
	}

	counts := make([][nrClasses]uint16, s.NBlocks)
	for i := range counts {
		for j := range counts[i] {
			counts[i][j] = binary.LittleEndian.Uint16(raw[(i*stride+j)*2:])
		}
	}

	f := &frame{
		m:             &blockMap{firstPFN: s.FirstPFN, nblocks: s.NBlocks, counts: counts},
		pagesPerBlock: s.PagesPerBlock,
		host:          s.Host,
		snapshot:      s.Label,
		kernel:        s.Kernel,
		taken:         s.Taken,
		viewTo:        s.NBlocks,
		scanned:       1,
		ladder:        freeLadder{orders: s.Ladder, maxOrder: s.MaxOrder},
		types:         blockTypes{names: s.TypeNames, counts: s.TypeCounts},
		mem:           s.Mem,
		vm:            s.Vm,
		arc:           s.Arc,
		whoErr:        s.WhoErr,
		whoTot: whoTotals{s.WhoTot.Blocks, s.WhoTot.HostageBlocks,
			s.WhoTot.HostageFreePages, s.WhoTot.WalkUS},
		hoverCol: -1,
		hoverRow: -1,
	}

	for _, c := range s.Slabs {
		f.slabs = append(f.slabs, slabCache{c.Name, c.Active, c.Total,
			c.ObjSize, c.PagesPerSlab, c.Slabs, c.ActiveSlabs})
	}
	if s.Who != nil {
		f.who = map[string]whoStat{}
		for k, w := range s.Who {
			f.who[k] = whoStat{w.Blocks, w.HostageBlocks, w.Pages, w.HostagePages, w.Mobile}
		}
	}
	// The class names depend on whether the machine that took the snapshot ran
	// ZFS, and that is a property of the snapshot rather than of this machine.
	nameClassesForHost(len(s.Arc) > 0)
	return f, nil
}
