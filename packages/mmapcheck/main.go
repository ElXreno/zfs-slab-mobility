// Readahead into the page cache of a ZFS file: whether a batch can hang against
// a writer, and whether the bytes it fills are the bytes the file holds.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	page          = 4096
	fadvWillneed  = 3
	fadvDontneed  = 4
	madvRandom    = 1
	madvDontneed  = 4
	deadlockSize  = 64 << 20
	deadlockWrite = 64 << 10
	deadlockSpan  = 4 << 20
)

func fadvise(fd int, off, length int64, advice int) error {
	_, _, e := syscall.Syscall6(syscall.SYS_FADVISE64, uintptr(fd), uintptr(off),
		uintptr(length), uintptr(advice), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

func report(out *os.File, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format+"\n", args...)
	_ = out.Sync()
}

// Writers rewrite 64K at random offsets while readers drop a 4M span from the
// page cache, fault one page of it back through a MADV_RANDOM mapping so the
// file keeps cached pages, and ask for the span with WILLNEED.
func deadlock(path, result string, writers, readers, seconds, stall int) error {
	out, err := os.Create(result)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	fd := int(f.Fd())
	fill := make([]byte, 1<<20)
	for i := range fill {
		fill[i] = 'f'
	}
	for off := int64(0); off < deadlockSize; off += int64(len(fill)) {
		if _, err := f.WriteAt(fill, off); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	m, err := syscall.Mmap(fd, 0, deadlockSize, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	if err := syscall.Madvise(m, madvRandom); err != nil {
		return err
	}

	var writes, readaheads atomic.Uint64
	for w := range writers {
		go func() {
			r := rand.New(rand.NewPCG(uint64(w)+1, 0x9e3779b97f4a7c15))
			buf := make([]byte, deadlockWrite)
			for i := range buf {
				buf[i] = 'w'
			}
			for {
				off := r.Int64N((deadlockSize-deadlockWrite)/page) * page
				if _, err := f.WriteAt(buf, off); err != nil {
					report(out, "ERROR pwrite: %v", err)
					os.Exit(1)
				}
				writes.Add(1)
			}
		}()
	}
	var sink atomic.Uint64
	for rd := range readers {
		go func() {
			r := rand.New(rand.NewPCG(uint64(rd)+101, 0xbf58476d1ce4e5b9))
			for {
				off := r.Int64N((deadlockSize-deadlockSpan)/page) * page
				_ = syscall.Madvise(m[off:off+deadlockSpan], madvDontneed)
				_ = fadvise(fd, off, deadlockSpan, fadvDontneed)
				sink.Add(uint64(m[off]))
				_ = fadvise(fd, off, deadlockSpan, fadvWillneed)
				readaheads.Add(1)
			}
		}()
	}

	var lastW, lastR uint64
	still := 0
	for t := 1; t <= seconds; t++ {
		time.Sleep(time.Second)
		w, r := writes.Load(), readaheads.Load()
		report(out, "%ds writes %d (+%d) readaheads %d (+%d)", t, w, w-lastW, r, r-lastR)
		if w == lastW && r == lastR {
			still++
		} else {
			still = 0
		}
		if still >= stall {
			report(out, "STALLED for %ds at %ds: writes %d readaheads %d", still, t, w, r)
			select {}
		}
		lastW, lastR = w, r
	}
	report(out, "OK %ds writes %d readaheads %d", seconds, writes.Load(), readaheads.Load())
	return nil
}

func majorFaults() int64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return ru.Majflt
}

// The sha256 of each file read through a shared mapping, with the major faults
// it took, so that the same digest through read() tells whether readahead
// filled the pages with the file's bytes.
func sum(paths []string) error {
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err != nil {
			return err
		}
		h := sha256.New()
		before := majorFaults()
		if size := st.Size(); size > 0 {
			m, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			h.Write(m)
			_ = syscall.Munmap(m)
		}
		_ = f.Close()
		fmt.Printf("%s  %s  %d\n", hex.EncodeToString(h.Sum(nil)), path, majorFaults()-before)
	}
	return nil
}

// A 512 byte write held in the read-modify-write of a cold record while
// WILLNEED reads that record ahead, then the page written into compared,
// through the mapping and through pread(), with what the file has to hold.
// The page that makes WILLNEED reach the page cache is in the next record,
// so the first one stays cold for the write.
func subpage(path, orig, inject string, holdMs int) error {
	const (
		record = 128 << 10
		span   = 2 * record
		at     = 1000
		size   = 512
	)
	want, err := os.ReadFile(orig)
	if err != nil {
		return err
	}
	for i := at; i < at+size; i++ {
		want[i] = 'W'
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd())
	m, err := syscall.Mmap(fd, 0, span, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Munmap(m) }()
	if err := syscall.Madvise(m, madvRandom); err != nil {
		return err
	}
	var sink atomic.Uint64
	sink.Add(uint64(m[span-page]))
	resident := []byte{0}
	if _, _, e := syscall.Syscall(syscall.SYS_MINCORE, uintptr(unsafe.Pointer(&m[span-page])),
		page, uintptr(unsafe.Pointer(&resident[0]))); e != 0 {
		return e
	}

	run := func(cmd string) error {
		args := strings.Fields(cmd)
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", cmd, err, out)
		}
		return nil
	}
	if err := run(inject); err != nil {
		return err
	}
	pool := strings.Fields(inject)[len(strings.Fields(inject))-1]

	t0 := time.Now()
	done := make(chan time.Duration, 1)
	go func() {
		buf := make([]byte, size)
		for i := range buf {
			buf[i] = 'W'
		}
		if _, err := f.WriteAt(buf, at); err != nil {
			fmt.Fprintln(os.Stderr, "pwrite:", err)
			os.Exit(1)
		}
		done <- time.Since(t0)
	}()
	time.Sleep(time.Duration(holdMs/4) * time.Millisecond)
	fadviseAt := time.Since(t0)
	if err := fadvise(fd, 0, span, fadvWillneed); err != nil {
		return err
	}
	fadviseDone := time.Since(t0)
	writer := <-done
	if err := run("zinject -c all"); err != nil {
		return err
	}

	got := make([]byte, page)
	if _, err := f.ReadAt(got, 0); err != nil {
		return err
	}
	diff := func(b []byte) int {
		n := 0
		for i := range page {
			if b[i] != want[i] {
				n++
			}
		}
		return n
	}
	fmt.Printf("{\"pool\": %q, \"cached\": %t, \"writer_ms\": %d, \"fadvise_at_ms\": %d, \"fadvise_done_ms\": %d, \"pread_wrong\": %d, \"mmap_wrong\": %d}\n",
		pool, resident[0]&1 == 1, writer.Milliseconds(), fadviseAt.Milliseconds(), fadviseDone.Milliseconds(), diff(got), diff(m[:page]))
	return nil
}

func main() {
	writers := flag.Int("writers", 4, "deadlock: threads rewriting the file")
	readers := flag.Int("readers", 4, "deadlock: threads dropping and prefetching spans")
	seconds := flag.Int("seconds", 120, "deadlock: how long to run")
	stall := flag.Int("stall", 20, "deadlock: seconds without progress that count as a hang")
	result := flag.String("result", "/run/mmapcheck.result", "deadlock: progress and verdict")
	orig := flag.String("orig", "", "subpage: copy of the file kept outside the pool")
	inject := flag.String("inject", "", "subpage: zinject command that delays reads of the pool")
	hold := flag.Int("hold", 1000, "subpage: read delay the inject command sets, ms")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mmapcheck [flags] deadlock FILE | sum FILE... | subpage FILE\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	var err error
	switch {
	case len(args) == 2 && args[0] == "deadlock":
		err = deadlock(args[1], *result, *writers, *readers, *seconds, *stall)
	case len(args) >= 2 && args[0] == "sum":
		err = sum(args[1:])
	case len(args) == 2 && args[0] == "subpage" && *orig != "" && *inject != "":
		err = subpage(args[1], *orig, *inject, *hold)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
