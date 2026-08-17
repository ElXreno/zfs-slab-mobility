package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// The offline path is the one a test guest feeds, and it fails silently when it
// fails at all: a wrong path yields an empty map rather than an error, and the
// numbers that come out of it look plausible.
func TestReadFromCollectedProc(t *testing.T) {
	dir := t.TempDir()
	const pages = 4096

	// 16 MiB of "RAM", which is eight pageblocks of 512 pages.
	if err := os.WriteFile(filepath.Join(dir, "iomem"),
		[]byte("00000000-00ffffff : System RAM\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	flags := make([]byte, pages*8)
	for i := range pages {
		var f uint64
		switch {
		case i < 512:
			f = 1 << kpfSlab
		case i < 1024:
			f = 1 << kpfBuddy
		default:
			f = 1 << kpfLRU
		}
		binary.LittleEndian.PutUint64(flags[i*8:], f)
	}
	if err := os.WriteFile(filepath.Join(dir, "kpageflags"), flags, 0o644); err != nil {
		t.Fatal(err)
	}

	old := procRoot
	procRoot = dir
	defer func() { procRoot = old }()

	m, err := newBlockMap(512)
	if err != nil {
		t.Fatal(err)
	}
	if m.nblocks != 8 {
		t.Fatalf("expected 8 pageblocks, got %d", m.nblocks)
	}

	dst := make([][nrClasses]uint16, m.nblocks)
	if err := m.refreshInto(0, m.nblocks, 512, dst, make([]byte, 1<<16)); err != nil {
		t.Fatal(err)
	}

	if got := dst[0][clSlab]; got != 512 {
		t.Errorf("first block: slab pages %d, expected 512", got)
	}
	if got := dst[1][clFree]; got != 512 {
		t.Errorf("second block: free pages %d, expected 512", got)
	}
	if got := dst[7][clFile]; got != 512 {
		t.Errorf("last block: cache pages %d, expected 512", got)
	}
}
