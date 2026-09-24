// A model loader meeting a page cache full of the previous model: large files
// streamed through mmap by many threads while shared memory is taken in a burst.
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

const (
	chunk = 1 << 20
	span  = 64 << 20
	page  = 4096
)

func fill(buf []byte, seed uint64, file, c int) {
	r := rand.NewPCG(seed, uint64(file)<<32|uint64(c))
	for i := 0; i+8 <= len(buf); i += 8 {
		binary.LittleEndian.PutUint64(buf[i:], r.Uint64())
	}
}

func name(dir string, file int) string {
	return filepath.Join(dir, fmt.Sprintf("f%03d", file))
}

func write(dir string, files int, size int64, seed uint64, jobs int) (uint64, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	var next atomic.Int64
	var total atomic.Uint64
	var wg sync.WaitGroup
	errs := make(chan error, jobs)

	for j := 0; j < jobs; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, chunk)
			for {
				i := int(next.Add(1)) - 1
				if i >= files {
					return
				}
				f, err := os.Create(name(dir, i))
				if err != nil {
					errs <- err
					return
				}
				for c := 0; int64(c)*chunk < size; c++ {
					fill(buf, seed, i, c)
					if _, err := f.Write(buf); err != nil {
						f.Close()
						errs <- err
						return
					}
					total.Add(chunk)
				}
				if err := f.Close(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	return total.Load(), <-errs
}

// Every file mapped shared and read-only, then copied out span by span from
// all threads at once, the way safetensors are copied into pinned buffers.
func mapRead(dir string, files, jobs int, hold time.Duration) (uint64, error) {
	maps := make([][]byte, files)
	for i := range maps {
		f, err := os.Open(name(dir, i))
		if err != nil {
			return 0, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return 0, err
		}
		m, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
		f.Close()
		if err != nil {
			return 0, fmt.Errorf("mmap %s: %w", name(dir, i), err)
		}
		maps[i] = m
	}
	defer func() {
		for _, m := range maps {
			syscall.Munmap(m)
		}
	}()

	type work struct{ file, off int }
	var spans []work
	for off := 0; ; off += span {
		more := false
		for i, m := range maps {
			if off < len(m) {
				spans = append(spans, work{i, off})
				more = true
			}
		}
		if !more {
			break
		}
	}

	var next atomic.Int64
	var total atomic.Uint64
	var wg sync.WaitGroup
	for j := 0; j < jobs; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, chunk)
			for {
				k := int(next.Add(1)) - 1
				if k >= len(spans) {
					return
				}
				m := maps[spans[k].file]
				end := min(spans[k].off+span, len(m))
				for off := spans[k].off; off < end; off += chunk {
					total.Add(uint64(copy(buf, m[off:min(off+chunk, end)])))
				}
			}
		}()
	}
	wg.Wait()
	time.Sleep(hold)
	return total.Load(), nil
}

// Shared memory taken as fast as the threads can fault it in, then held.
func shm(mb, jobs int, hold time.Duration) (uint64, error) {
	size := mb << 20
	path := fmt.Sprintf("/dev/shm/fragspike-%d", os.Getpid())
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	os.Remove(path)
	defer f.Close()
	if err := f.Truncate(int64(size)); err != nil {
		return 0, err
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return 0, fmt.Errorf("mmap: %w", err)
	}
	defer syscall.Munmap(mem)

	var wg sync.WaitGroup
	per := (size/jobs + page - 1) / page * page
	for j := 0; j < jobs; j++ {
		wg.Add(1)
		go func(lo int) {
			defer wg.Done()
			for off := lo; off < min(lo+per, size); off += page {
				mem[off] = 1
			}
		}(j * per)
	}
	wg.Wait()
	time.Sleep(hold)
	return uint64(size), nil
}

func main() {
	mode := flag.String("mode", "map", "write, map or shm")
	dir := flag.String("dir", "", "directory holding the files")
	files := flag.Int("files", 4, "how many files")
	mb := flag.Int("mb", 1024, "size of one file, or of the shared memory, MiB")
	seed := flag.Uint64("seed", 20260924, "seed for the contents")
	jobs := flag.Int("jobs", runtime.NumCPU(), "worker threads")
	secs := flag.Int("secs", 0, "how long to hold the mappings afterwards, seconds")
	flag.Parse()

	start := time.Now()
	hold := time.Duration(*secs) * time.Second
	var moved uint64
	var err error

	switch *mode {
	case "write":
		moved, err = write(*dir, *files, int64(*mb)<<20, *seed, *jobs)
	case "map":
		moved, err = mapRead(*dir, *files, *jobs, hold)
	case "shm":
		moved, err = shm(*mb, *jobs, hold)
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fragspike:", err)
		os.Exit(1)
	}

	el := time.Since(start).Seconds()
	fmt.Printf("%s: %.1f GiB in %.1f s (%.0f MiB/s)\n", *mode,
		float64(moved)/(1<<30), el, float64(moved)/(1<<20)/el)
}
