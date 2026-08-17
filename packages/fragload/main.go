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
	"time"
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

func main() {
	mode := flag.String("mode", "write", "write, read or hot")
	dir := flag.String("dir", "", "directory holding the set")
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
