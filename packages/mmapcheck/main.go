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
	"sync/atomic"
	"syscall"
	"time"
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
	fmt.Fprintf(out, format+"\n", args...)
	out.Sync()
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
				syscall.Madvise(m[off:off+deadlockSpan], madvDontneed)
				fadvise(fd, off, deadlockSpan, fadvDontneed)
				sink.Add(uint64(m[off]))
				fadvise(fd, off, deadlockSpan, fadvWillneed)
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
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
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
			syscall.Munmap(m)
		}
		f.Close()
		fmt.Printf("%s  %s  %d\n", hex.EncodeToString(h.Sum(nil)), path, majorFaults()-before)
	}
	return nil
}

func main() {
	writers := flag.Int("writers", 4, "deadlock: threads rewriting the file")
	readers := flag.Int("readers", 4, "deadlock: threads dropping and prefetching spans")
	seconds := flag.Int("seconds", 120, "deadlock: how long to run")
	stall := flag.Int("stall", 20, "deadlock: seconds without progress that count as a hang")
	result := flag.String("result", "/run/mmapcheck.result", "deadlock: progress and verdict")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: mmapcheck [flags] deadlock FILE | sum FILE...\n")
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
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
