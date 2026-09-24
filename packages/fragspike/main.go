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
	"unsafe"
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
						_ = f.Close()
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
// Only the leading frac of each file is read.
func mapRead(dir string, files, jobs int, frac float64, hold time.Duration) (uint64, error) {
	maps := make([][]byte, files)
	for i := range maps {
		f, err := os.Open(name(dir, i))
		if err != nil {
			return 0, err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return 0, err
		}
		m, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
		_ = f.Close()
		if err != nil {
			return 0, fmt.Errorf("mmap %s: %w", name(dir, i), err)
		}
		maps[i] = m
	}
	defer func() {
		for _, m := range maps {
			_ = syscall.Munmap(m)
		}
	}()

	type work struct{ file, off int }
	var spans []work
	for off := 0; ; off += span {
		more := false
		for i, m := range maps {
			if off < int(float64(len(m))*frac) {
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

// Shared anonymous memory is shmem without the size cap of /dev/shm.
func arena(mb int) ([]byte, error) {
	mem, err := syscall.Mmap(-1, 0, mb<<20, syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED|syscall.MAP_ANONYMOUS)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}
	return mem, nil
}

// Long-term pins the whole range the way cudaHostRegister does, through
// pin_user_pages with FOLL_LONGTERM: the pages stay on the LRU but cannot go.
// The pin lasts until the returned descriptor is closed.
func pin(mem []byte) (int, error) {
	const setup, register, registerBuffers = 425, 427, 0
	var params [120]byte
	fd, _, errno := syscall.Syscall(setup, 1, uintptr(unsafe.Pointer(&params[0])), 0)
	if errno != 0 {
		return -1, fmt.Errorf("io_uring_setup: %w", errno)
	}
	var iovs []syscall.Iovec
	for off := 0; off < len(mem); off += 1 << 30 {
		iov := syscall.Iovec{Base: &mem[off]}
		iov.SetLen(min(1<<30, len(mem)-off))
		iovs = append(iovs, iov)
	}
	_, _, errno = syscall.Syscall6(register, fd, registerBuffers,
		uintptr(unsafe.Pointer(&iovs[0])), uintptr(len(iovs)), 0, 0)
	if errno != 0 {
		_ = syscall.Close(int(fd))
		return -1, fmt.Errorf("io_uring_register: %w", errno)
	}
	return int(fd), nil
}

func touch(mem []byte, jobs int) {
	var wg sync.WaitGroup
	per := (len(mem)/jobs + page - 1) / page * page
	for j := 0; j < jobs; j++ {
		wg.Add(1)
		go func(lo int) {
			defer wg.Done()
			for off := lo; off < min(lo+per, len(mem)); off += page {
				mem[off] = 1
			}
		}(j * per)
	}
	wg.Wait()
}

// Shared memory taken as fast as the threads can fault it in, then held.
func shm(mb, jobs int, pinned bool, hold time.Duration) (uint64, error) {
	mem, err := arena(mb)
	if err != nil {
		return 0, err
	}
	defer func() { _ = syscall.Munmap(mem) }()
	touch(mem, jobs)
	if pinned {
		fd, err := pin(mem)
		if err != nil {
			return 0, err
		}
		defer func() { _ = syscall.Close(fd) }()
	}
	time.Sleep(hold)
	return uint64(len(mem)), nil
}

// A lazy private mapping asked for huge pages, the way a loader allocates a
// bank it only fills later: nothing is resident until the reads land in it.
func bank(mb int) ([]byte, error) {
	mem, err := syscall.Mmap(-1, 0, mb<<20, syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}
	_ = syscall.Madvise(mem, 14)
	return mem, nil
}

// Files read with pread straight into a fresh destination, so every first
// touch of it faults inside the filesystem's read. Buffered reads land in a
// shared arena other threads fault in as fast as they can; direct reads land
// in a lazy huge page bank in chunks of the loader's size.
func load(dir string, files, mb, jobs int, direct, pinned bool, hold time.Duration) (uint64, error) {
	var mem []byte
	var err error
	step := span
	flags := os.O_RDONLY
	if direct {
		mem, err = bank(mb)
		step = 8 << 20
		flags |= syscall.O_DIRECT
	} else {
		mem, err = arena(mb)
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = syscall.Munmap(mem) }()

	type work struct {
		f   *os.File
		off int64
		dst int
		n   int
	}
	var spans []work
	dst := 0
	for i := 0; i < files; i++ {
		f, err := os.OpenFile(name(dir, i), flags, 0)
		if err != nil {
			return 0, err
		}
		defer func() { _ = f.Close() }()
		st, err := f.Stat()
		if err != nil {
			return 0, err
		}
		for off := int64(0); off < st.Size(); off += int64(step) {
			spans = append(spans, work{f, off, dst % len(mem), step})
			dst += step
		}
	}

	var wg sync.WaitGroup
	if !direct {
		wg.Add(1)
		go func() {
			defer wg.Done()
			touch(mem, max(jobs/4, 1))
		}()
	}

	var next atomic.Int64
	var total atomic.Uint64
	errs := make(chan error, jobs)
	for j := 0; j < jobs; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				k := int(next.Add(1)) - 1
				if k >= len(spans) {
					return
				}
				w := spans[k]
				buf := mem[w.dst:min(w.dst+w.n, len(mem))]
				n, err := w.f.ReadAt(buf, w.off)
				if err != nil && n == 0 {
					errs <- fmt.Errorf("%s at %d: %w", w.f.Name(), w.off, err)
					return
				}
				total.Add(uint64(n))
			}
		}()
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return total.Load(), err
	}
	if pinned {
		fd, err := pin(mem)
		if err != nil {
			return total.Load(), err
		}
		defer func() { _ = syscall.Close(fd) }()
	}
	time.Sleep(hold)
	return total.Load(), nil
}

func main() {
	mode := flag.String("mode", "map", "write, map, shm or load")
	dir := flag.String("dir", "", "directory holding the files")
	files := flag.Int("files", 4, "how many files")
	mb := flag.Int("mb", 1024, "size of one file, or of the shared memory, MiB")
	seed := flag.Uint64("seed", 20260924, "seed for the contents")
	jobs := flag.Int("jobs", runtime.NumCPU(), "worker threads")
	secs := flag.Int("secs", 0, "how long to hold the mappings afterwards, seconds")
	frac := flag.Float64("frac", 1, "share of each file to map and read")
	direct := flag.Bool("direct", false, "load with O_DIRECT into a lazy huge page bank")
	pinned := flag.Bool("pin", false, "long-term pin the filled memory while it is held")
	flag.Parse()

	start := time.Now()
	hold := time.Duration(*secs) * time.Second
	var moved uint64
	var err error

	switch *mode {
	case "write":
		moved, err = write(*dir, *files, int64(*mb)<<20, *seed, *jobs)
	case "map":
		moved, err = mapRead(*dir, *files, *jobs, *frac, hold)
	case "shm":
		moved, err = shm(*mb, *jobs, *pinned, hold)
	case "load":
		moved, err = load(*dir, *files, *mb, *jobs, *direct, *pinned, hold)
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
