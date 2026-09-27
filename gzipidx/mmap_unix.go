//go:build unix

package gzipidx

import (
	"os"
	"syscall"
)

// mapFile maps [off, off+size) of f read-only so the scanner can decode the
// compressed bytes in place. Returns the mapping and an unmap function.
func mapFile(f *os.File, off, size int64) ([]byte, func(), error) {
	if size <= 0 || int64(int(size)) != size {
		return nil, nil, syscall.EINVAL
	}
	b, err := syscall.Mmap(int(f.Fd()), off, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}
	return b, func() { _ = syscall.Munmap(b) }, nil
}

// mapFileAvailable reports whether mapping is worth trying on this platform.
const mapFileAvailable = true
