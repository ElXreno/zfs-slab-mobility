package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
)

// Where to read the kernel's own reporting from.
//
// A test guest copies these files out verbatim rather than running any of this
// code, so a change to the classifier costs a rerun of the analysis and not a
// rerun of the virtual machines that produced the evidence.
var procRoot = "/proc"

func procPath(rel string) string { return filepath.Join(procRoot, rel) }

func openProc(rel string) (*os.File, error) { return os.Open(procPath(rel)) }

// The page flag file is read at offsets rather than in order, so a collected
// copy is loaded whole. It is a few megabytes, and gzip makes it a fraction of
// that in a store that does not compress.
type flagSource interface {
	ReadAt(p []byte, off int64) (int, error)
}

func openFlags() (flagSource, error) {
	if procRoot == "/proc" {
		return os.Open(procPath("kpageflags"))
	}

	if f, err := os.Open(procPath("kpageflags.gz")); err == nil {
		defer f.Close()
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		b, err := io.ReadAll(zr)
		if err != nil {
			return nil, err
		}
		return bytes.NewReader(b), nil
	}

	b, err := os.ReadFile(procPath("kpageflags"))
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(b), nil
}
