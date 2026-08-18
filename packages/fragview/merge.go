// Which caches actually share the kmem_cache a slab page belongs to.
//
// SLUB folds caches of compatible size and alignment into one and keeps the
// name of whichever was created first, so /proc/slabinfo and the kernel module
// both report that one name for all of them. Nothing records which alias an
// individual object came from, and no amount of work here can recover it: the
// honest answer is the whole set. On one desktop the cache reported as
// ftrace_event_field turned out to be holding zswap_entry, and the name sent
// the reading in the wrong direction entirely.
//
// Boot with slab_nomerge for per-cache truth.
package main

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const slabSysDir = "/sys/kernel/slab"

// Every alias of a merged cache, keyed by each of its members. Caches that were
// not merged are absent rather than mapped to themselves.
var cacheAliases map[string][]string

func loadCacheAliases() map[string][]string {
	byGroup := map[string][]string{}

	if procRoot == "/proc" {
		ents, err := os.ReadDir(slabSysDir)
		if err != nil {
			return nil
		}
		for _, e := range ents {
			if e.Type()&os.ModeSymlink == 0 {
				continue
			}
			target, err := os.Readlink(filepath.Join(slabSysDir, e.Name()))
			if err != nil {
				continue
			}
			g := filepath.Base(target)
			byGroup[g] = append(byGroup[g], e.Name())
		}
	} else {
		// Captured in the guest as "name target" per line, since a test VM
		// copies kernel reporting out rather than running any of this.
		f, err := openProc("slabmerge")
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fl := strings.Fields(sc.Text())
			if len(fl) != 2 {
				continue
			}
			g := filepath.Base(fl[1])
			byGroup[g] = append(byGroup[g], fl[0])
		}
	}

	out := map[string][]string{}
	for _, members := range byGroup {
		if len(members) < 2 {
			continue
		}
		sort.Strings(members)
		for _, m := range members {
			out[m] = members
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// The reported name with a count of the aliases hiding behind it, so a merged
// cache never reads as if it were the one thing it is named after.
func cacheLabel(name string) string {
	a := cacheAliases[name]
	if len(a) < 2 {
		return name
	}
	return name + "+" + itoa(len(a)-1)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Spells out the sets behind the names just printed. Only the caches that
// appeared are listed, so the legend stays as short as the table it explains.
func aliasLegend(shown []string) string {
	var lines []string
	seen := map[string]bool{}
	for _, n := range shown {
		a := cacheAliases[n]
		if len(a) < 2 || seen[a[0]] {
			continue
		}
		seen[a[0]] = true
		lines = append(lines, "  "+cacheLabel(n)+"  "+strings.Join(a, " "))
	}
	if len(lines) == 0 {
		return ""
	}
	return "\nmerged caches, one kmem_cache each: which alias owns an object is" +
		" not recorded anywhere.\nBoot with slab_nomerge to tell them apart.\n" +
		strings.Join(lines, "\n") + "\n"
}

// The same legend wrapped for a pane rather than a terminal report. Only the
// caches passed in are explained, so what is on screen is what gets spelled
// out.
func aliasLines(shown []string, width, max int) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range shown {
		a := cacheAliases[n]
		if len(a) < 2 || seen[a[0]] || len(out) >= max {
			continue
		}
		seen[a[0]] = true
		line := " " + cacheLabel(n) + " ="
		for _, m := range a {
			if len(line)+1+len(m) > width {
				out = append(out, line)
				if len(out) >= max {
					return out
				}
				line = "     "
			}
			line += " " + m
		}
		out = append(out, line)
	}
	return out
}
