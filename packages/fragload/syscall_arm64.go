//go:build linux && arm64

// The generic table rather than an architecture specific one: arm64 takes its
// numbers from include/uapi/asm-generic/unistd.h.
package main

const sysCopyFileRange = 285
