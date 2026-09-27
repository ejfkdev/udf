//go:build unix

package gzipidx

import (
	"errors"
	"os"
	"syscall"
)

// mapFile maps [off, off+size) of f read-only so the scanner can decode the
// compressed bytes in place. Returns the mapping and an unmap function.
//
// mmap requires a page-aligned offset while a gzip stream starts a few bytes
// into the file, so the mapping starts at the page boundary below off and the
// extra leading bytes are sliced off.
func mapFile(f *os.File, off, size int64) ([]byte, func(), error) {
	if size <= 0 || int64(int(size)) != size {
		return nil, nil, errors.New("invalid mapping size")
	}
	page := int64(os.Getpagesize())
	base := off &^ (page - 1)
	pad := off - base
	if int64(int(pad+size)) != pad+size {
		return nil, nil, errors.New("mapping too large for this platform")
	}
	b, err := syscall.Mmap(int(f.Fd()), base, int(pad+size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return b[pad:], func() { _ = syscall.Munmap(b) }, nil
}

// mapFileAvailable reports whether mapping is worth trying on this platform.
const mapFileAvailable = true
