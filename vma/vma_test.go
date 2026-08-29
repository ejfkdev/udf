package vma

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenFileExtractsDevice(t *testing.T) {
	// Three clusters: full, sparse-zero, and partially populated (first 4 KiB).
	guest := make([]byte, 3*clusterSize)
	for i := range guest[:clusterSize] {
		guest[i] = 0xAB
	}
	for i := range guest[2*clusterSize : 2*clusterSize+4096] {
		guest[2*clusterSize+i] = 0xCD
	}
	path := writeVMA(t, "disk-0.raw", guest)

	img, err := OpenFile(path)
	if err != nil {
		t.Fatalf("open vma: %v", err)
	}
	defer img.Close()
	if len(img.Disks) != 1 {
		t.Fatalf("expected 1 disk, got %d", len(img.Disks))
	}
	d := img.Disks[0]
	if d.Name != "disk-0.raw" {
		t.Fatalf("unexpected disk name %q", d.Name)
	}
	if d.Size() != int64(len(guest)) {
		t.Fatalf("unexpected size %d", d.Size())
	}
	got := make([]byte, d.Size())
	if _, err := d.ReadAt(got, 0); err != nil && err.Error() != "EOF" {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, guest) {
		t.Fatalf("extracted data mismatch")
	}
}

func TestOpenRejectsNonVma(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not.vma")
	if err := os.WriteFile(path, []byte("not a vma"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := OpenFile(path); err == nil {
		t.Fatal("expected non-vma data to be rejected")
	}
}

// writeVMA builds a minimal single-device VMA whose sole device holds guest.
// It encodes the guest as full/sparse/partial clusters so all mask forms are
// exercised.
func writeVMA(t *testing.T, name string, guest []byte) string {
	t.Helper()

	devName := name + "\x00"
	// The blob buffer is one padding byte, then a little-endian length and the
	// name; blobs are keyed by their length-prefix offset, so the first blob's
	// key (and the device name offset) is 1, matching vma_spec.txt.
	const fixedHeaderSize = 12288
	blobBufferSize := 1 + 2 + len(devName)
	headerSize := fixedHeaderSize + blobBufferSize

	// Extent: one blockInfo per cluster.
	nClusters := (len(guest) + clusterSize - 1) / clusterSize
	extent := make([]byte, 512)
	copy(extent[0:4], "VMAE")
	binary.BigEndian.PutUint16(extent[6:8], uint16(nClusters))

	var data bytes.Buffer
	for c := 0; c < nClusters; c++ {
		start := c * clusterSize
		end := start + clusterSize
		if end > len(guest) {
			end = len(guest)
		}

		mask := uint16(0)
		for i := 0; i < 16; i++ {
			chunkStart := start + i*4096
			if chunkStart >= end {
				break
			}
			chunkEnd := chunkStart + 4096
			if chunkEnd > end {
				chunkEnd = end
			}
			if !allZero(guest[chunkStart:chunkEnd]) {
				mask |= 1 << i
				data.Write(guest[chunkStart:chunkEnd])
			}
		}
		if mask == 0 {
			// sparse zero cluster: mask 0.
		} else if mask == 0xFFFF {
			// still write the full cluster uncompressed.
		}

		bi := extent[40+c*8 : 40+c*8+8]
		binary.BigEndian.PutUint16(bi[0:2], mask)
		bi[3] = 1 // dev id
		binary.BigEndian.PutUint32(bi[4:8], uint32(c))
	}

	// Assemble file.
	var out bytes.Buffer
	hdr := make([]byte, headerSize)
	copy(hdr[0:4], "VMA\x00")
	binary.BigEndian.PutUint32(hdr[4:8], 1)
	binary.BigEndian.PutUint32(hdr[48:52], fixedHeaderSize) // blob_buffer_offset
	binary.BigEndian.PutUint32(hdr[52:56], uint32(blobBufferSize))
	binary.BigEndian.PutUint32(hdr[56:60], uint32(headerSize))
	// device entry 1 at offset 4096+32 (dev id 0 is reserved as "empty").
	dev := hdr[4096+32 : 4096+64]
	binary.BigEndian.PutUint32(dev[0:4], 1) // device name offset = blob key 1
	binary.BigEndian.PutUint64(dev[8:16], uint64(len(guest)))
	// blob buffer: [0]=padding, [1:3]=len (LE), [3:]=name.
	hdr[fixedHeaderSize] = 0
	binary.LittleEndian.PutUint16(hdr[fixedHeaderSize+1:fixedHeaderSize+3], uint16(len(devName)))
	copy(hdr[fixedHeaderSize+3:fixedHeaderSize+3+len(devName)], devName)

	out.Write(hdr)
	out.Write(extent)
	out.Write(data.Bytes())

	path := filepath.Join(t.TempDir(), "test.vma")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatalf("write vma: %v", err)
	}
	return path
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
