// What a run asserts on rather than what it loads: whether the set fragload
// wrote still reads back byte for byte, and a process that needs memory while
// the cache holds it. Kept apart from fragload so that a run which needs
// neither keeps the guest it already measured.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The same derivation fragload uses, so a file's expected contents come from
// its index and the seed alone.
func fill(buf []byte, seed uint64, idx int) {
	r := rand.NewPCG(seed, uint64(idx)+0x9E3779B97F4A7C15)
	for i := 0; i+8 <= len(buf); i += 8 {
		binary.LittleEndian.PutUint64(buf[i:], r.Uint64())
	}
}

func path(dir string, idx, perDir int) string {
	return filepath.Join(dir, fmt.Sprintf("d%03d", idx/perDir), fmt.Sprintf("f%06d", idx))
}

// Reads every file back and compares it with what fill() says it should hold.
// The first mismatch is reported with its offset and the count goes on, so a
// run says how much was damaged rather than only that something was.
func verify(dir string, files, size int, seed uint64, perDir, jobs int) (uint64, error) {
	var total atomic.Uint64
	var bad atomic.Int64
	var first atomic.Value
	var next atomic.Int64
	var wg sync.WaitGroup

	for j := 0; j < jobs; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := make([]byte, size)
			for {
				i := int(next.Add(1)) - 1
				if i >= files {
					return
				}
				name := path(dir, i, perDir)
				got, err := os.ReadFile(name)
				if err != nil {
					bad.Add(1)
					first.CompareAndSwap(nil, fmt.Errorf("%s: %w", name, err))
					continue
				}
				total.Add(uint64(len(got)))
				if len(got) != size {
					bad.Add(1)
					first.CompareAndSwap(nil, fmt.Errorf("%s: %d bytes, expected %d", name, len(got), size))
					continue
				}
				fill(want, seed, i)
				for off := 0; off < size; off++ {
					if got[off] != want[off] {
						bad.Add(1)
						first.CompareAndSwap(nil, fmt.Errorf("%s: byte %d is %#02x, expected %#02x",
							name, off, got[off], want[off]))
						break
					}
				}
			}
		}()
	}
	wg.Wait()

	if n := bad.Load(); n > 0 {
		return total.Load(), fmt.Errorf("%d of %d files differ, first: %v", n, files, first.Load())
	}
	return total.Load(), nil
}

// Anonymous memory that stays in use: every page is written once and then
// touched again on every pass, which is what a process the kernel should be
// keeping resident looks like beside a cache it should be shrinking.
func hog(mb int, dur time.Duration) (uint64, error) {
	const page = 4096
	size := mb << 20
	mem, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		return 0, fmt.Errorf("mmap: %w", err)
	}
	var touched uint64
	deadline := time.Now().Add(dur)
	for pass := byte(1); ; pass++ {
		for off := 0; off < size; off += page {
			mem[off] = pass
			touched += page
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	return touched, nil
}

func main() {
	mode := flag.String("mode", "verify", "verify or hog")
	dir := flag.String("dir", "", "directory holding the set")
	files := flag.Int("files", 100000, "how many files")
	size := flag.Int("size", 128*1024, "size of one file")
	seedStr := flag.Uint64("seed", 20260817, "seed for the set")
	perDir := flag.Int("per-dir", 1000, "files per subdirectory")
	jobs := flag.Int("jobs", runtime.NumCPU(), "worker threads")
	mb := flag.Int("mb", 1024, "anonymous memory to hold, MiB")
	secs := flag.Int("secs", 60, "how long to hold it, seconds")
	flag.Parse()

	start := time.Now()
	var moved uint64
	var err error

	switch *mode {
	case "verify":
		if *dir == "" {
			err = fmt.Errorf("-dir is required for verify")
			break
		}
		moved, err = verify(*dir, *files, *size, *seedStr, *perDir, *jobs)
	case "hog":
		moved, err = hog(*mb, time.Duration(*secs)*time.Second)
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fragcheck:", err)
		os.Exit(1)
	}

	el := time.Since(start).Seconds()
	fmt.Printf("%s: %.1f GiB in %.1f s (%.0f MiB/s)\n", *mode,
		float64(moved)/(1<<30), el, float64(moved)/(1<<20)/el)
}
