//go:build linux && amd64

// Go's frozen syscall package never got a name for this one, and pulling in
// x/sys for a single number would mean vendoring a module into a build that
// otherwise has no dependencies at all. The number comes from the kernel's own
// table, arch/x86/entry/syscalls/syscall_64.tbl.
package main

const sysCopyFileRange = 326
