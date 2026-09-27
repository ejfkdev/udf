//go:build !unix

package gzipidx

import (
	"errors"
	"os"
)

// mapFile is unavailable on this platform; the scanner falls back to buffered
// reads of the compressed stream.
func mapFile(f *os.File, off, size int64) ([]byte, func(), error) {
	return nil, nil, errors.New("file mapping not supported")
}

const mapFileAvailable = false
