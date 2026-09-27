//go:build unix

package gzipidx

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// TestMapFileOffsets checks that a mapping can start at an unaligned offset
// (a gzip stream begins a few bytes into the file) and that the mapped bytes
// appear at position zero of the returned slice.
func TestMapFileOffsets(t *testing.T) {
	if !mapFileAvailable {
		t.Skip("no mapping on this platform")
	}
	rng := rand.New(rand.NewSource(5))
	data := make([]byte, 1<<20+123)
	rng.Read(data)
	path := filepath.Join(t.TempDir(), "raw.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for _, off := range []int64{0, 1, 10, 4095, 4096, 4097, 100003} {
		size := int64(len(data)) - off
		got, unmap, err := mapFile(f, off, size)
		if err != nil {
			t.Fatalf("off=%d: %v", off, err)
		}
		if len(got) != int(size) {
			t.Fatalf("off=%d: mapped %d bytes, want %d", off, len(got), size)
		}
		if !bytes.Equal(got, data[off:]) {
			t.Fatalf("off=%d: mapped bytes differ from the file", off)
		}
		unmap()
	}
}
