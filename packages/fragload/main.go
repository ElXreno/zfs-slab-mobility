// Deterministic file load for the fragmentation stand.
//
// One big file gives the ARC plenty of data and almost no objects, and the ZFS
// slab caches that hold pageblocks hostage grow with the number of objects:
// dnode_t, dmu_buf_impl_t and arc_buf_hdr_t_full are per dnode, per dbuf and
// per buffer. So the load is many small files instead, one record each, which
// gives both halves at once.
//
// Everything derives from a seed, so two runs of the same parameters lay down
// the same bytes in the same order and read them back in the same order.
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
	"unsafe"
)

// A file's contents come from its index alone, so writing is embarrassingly
// parallel and a reader can verify without keeping anything.
func fill(buf []byte, seed uint64, idx int) {
	r := rand.NewPCG(seed, uint64(idx)+0x9E3779B97F4A7C15)
	for i := 0; i+8 <= len(buf); i += 8 {
		binary.LittleEndian.PutUint64(buf[i:], r.Uint64())
	}
}

func path(dir string, idx, perDir int) string {
	return filepath.Join(dir, fmt.Sprintf("d%03d", idx/perDir), fmt.Sprintf("f%06d", idx))
}

func write(dir string, files, size int, seed uint64, perDir, jobs int) error {
	for i := 0; i < (files+perDir-1)/perDir; i++ {
		if err := os.MkdirAll(filepath.Join(dir, fmt.Sprintf("d%03d", i)), 0o755); err != nil {
			return err
		}
	}

	var next atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, jobs)

	for j := 0; j < jobs; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, size)
			for {
				i := int(next.Add(1)) - 1
				if i >= files {
					return
				}
				fill(buf, seed, i)
				if err := os.WriteFile(path(dir, i, perDir), buf, 0o644); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	return <-errs
}

// Reads every file once per pass, in an order that depends on the seed and the
// pass number. Sequential order would let the prefetcher answer most of it and
// the ARC would never hold much at once.
func read(dir string, files, size int, seed uint64, perDir, jobs, passes int) (uint64, error) {
	order := make([]int, files)
	for i := range order {
		order[i] = i
	}

	var total atomic.Uint64
	for p := 0; p < passes; p++ {
		r := rand.New(rand.NewPCG(seed, uint64(p)+1))
		r.Shuffle(len(order), func(a, b int) { order[a], order[b] = order[b], order[a] })

		var next atomic.Int64
		var wg sync.WaitGroup
		errs := make(chan error, jobs)

		for j := 0; j < jobs; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				buf := make([]byte, size)
				for {
					k := int(next.Add(1)) - 1
					if k >= files {
						return
					}
					f, err := os.Open(path(dir, order[k], perDir))
					if err != nil {
						errs <- err
						return
					}
					n, _ := f.Read(buf)
					f.Close()
					total.Add(uint64(n))
				}
			}()
		}
		wg.Wait()
		close(errs)
		if err := <-errs; err != nil {
			return total.Load(), err
		}
	}
	return total.Load(), nil
}

// Keeps a slice of the set hot for a while, so that what survives a squeeze is
// data somebody is holding rather than metadata describing data nobody wants.
func hot(dir string, files, size int, seed uint64, perDir, slice int, dur time.Duration) uint64 {
	var total uint64
	buf := make([]byte, size)
	deadline := time.Now().Add(dur)
	for pass := 0; time.Now().Before(deadline); pass++ {
		for k := 0; k < slice && k < files; k++ {
			f, err := os.Open(path(dir, k, perDir))
			if err != nil {
				continue
			}
			n, _ := f.Read(buf)
			f.Close()
			total += uint64(n)
		}
	}
	return total
}

// One copy_file_range call at a time, with explicit offsets so a short copy is
// visible rather than hidden behind the file position. On a pool with block
// cloning this lands in zfs_clone_range and clones block pointers instead of
// moving bytes, which is the path a build's cp takes when the build directory
// and the store share a pool.
func cloneRange(src, dst *os.File, size int64) error {
	for off := int64(0); off < size; {
		inOff, outOff := off, off
		n, _, errno := syscall.Syscall6(sysCopyFileRange,
			src.Fd(), uintptr(unsafe.Pointer(&inOff)),
			dst.Fd(), uintptr(unsafe.Pointer(&outOff)),
			uintptr(size-off), 0)
		if errno != 0 {
			return fmt.Errorf("copy_file_range at %d: %w", off, errno)
		}
		if n == 0 {
			return fmt.Errorf("copy_file_range at %d returned 0 of %d",
				off, size-off)
		}
		off += int64(n)
	}
	return nil
}

// Clones the set from one dataset into another for a while. The failure being
// hunted is an error out of the call itself, so the first one stops everything
// and is reported with the file and the errno: coreutils does not fall back
// from EIO here, and neither does this.
func clone(dir, dest string, files, size, perDir, jobs int, dur time.Duration) (uint64, error) {
	var total atomic.Uint64
	var failed atomic.Value
	deadline := time.Now().Add(dur)

	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				for i := worker; i < files; i += jobs {
					if failed.Load() != nil ||
						!time.Now().Before(deadline) {
						return
					}
					if err := cloneOne(dir, dest, i, size,
						perDir, worker); err != nil {
						failed.Store(err)
						return
					}
					total.Add(uint64(size))
				}
			}
		}(w)
	}
	wg.Wait()

	if err, ok := failed.Load().(error); ok && err != nil {
		return total.Load(), err
	}
	return total.Load(), nil
}

func cloneOne(dir, dest string, idx, size, perDir, worker int) error {
	src, err := os.Open(path(dir, idx, perDir))
	if err != nil {
		return err
	}
	defer src.Close()

	out := filepath.Join(dest, fmt.Sprintf("w%02d", worker))
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	// A fresh inode every round rather than a rewrite of the last one: this
	// is what a build does, and cloning over an existing file would be a
	// different path with its own known bugs. Unlinking first keeps the
	// destination from growing without bound over a long run.
	name := filepath.Join(out, fmt.Sprintf("f%06d", idx))
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	dst, err := os.Create(name)
	if err != nil {
		return err
	}
	defer dst.Close()

	if err := cloneRange(src, dst, int64(size)); err != nil {
		return fmt.Errorf("%s: %w", src.Name(), err)
	}
	return nil
}

func main() {
	mode := flag.String("mode", "write", "write, read, hot or clone")
	dir := flag.String("dir", "", "directory holding the set")
	dest := flag.String("dest", "", "destination directory for clone, on another dataset")
	files := flag.Int("files", 100000, "how many files")
	size := flag.Int("size", 128*1024, "size of one file")
	seedStr := flag.Uint64("seed", 20260817, "seed for the set")
	perDir := flag.Int("per-dir", 1000, "files per subdirectory")
	passes := flag.Int("passes", 1, "read passes")
	slice := flag.Int("slice", 2048, "files in the hot slice")
	secs := flag.Int("secs", 60, "how long to keep it hot, seconds")
	jobs := flag.Int("jobs", runtime.NumCPU(), "worker threads")
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "fragload: -dir is required")
		os.Exit(1)
	}
	start := time.Now()
	var moved uint64
	var err error

	switch *mode {
	case "write":
		err = write(*dir, *files, *size, *seedStr, *perDir, *jobs)
		moved = uint64(*files) * uint64(*size)
	case "read":
		moved, err = read(*dir, *files, *size, *seedStr, *perDir, *jobs, *passes)
	case "hot":
		moved = hot(*dir, *files, *size, *seedStr, *perDir, *slice, time.Duration(*secs)*time.Second)
	case "clone":
		if *dest == "" {
			err = fmt.Errorf("-dest is required for clone")
			break
		}
		moved, err = clone(*dir, *dest, *files, *size, *perDir, *jobs,
			time.Duration(*secs)*time.Second)
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fragload:", err)
		os.Exit(1)
	}

	el := time.Since(start).Seconds()
	fmt.Printf("%s: %.1f GiB in %.1f s (%.0f MiB/s)\n", *mode,
		float64(moved)/(1<<30), el, float64(moved)/(1<<20)/el)
}
