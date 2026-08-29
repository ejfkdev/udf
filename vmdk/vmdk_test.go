package vmdk

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// writeMonolithicSparse builds a small, uncompressed monolithicSparse VMDK in
// memory whose grains hold the raw guest bytes in order, with any zero guest
// sector left as an unallocated grain.
func writeMonolithicSparse(t *testing.T, guest []byte, grainSectors int64) []byte {
	t.Helper()

	const (
		numGTEsPerGT = 128
		hdrSector    = 0
		gdSector     = 1
		gtSector     = 2
		dataSector   = 3
	)

	grainBytes := grainSectors * sectorSize
	totalSectors := (int64(len(guest)) + sectorSize - 1) / sectorSize
	gtes := (totalSectors + grainSectors - 1) / grainSectors

	size := int64(dataSector) + gtes*grainSectors
	buf := make([]byte, size*sectorSize)

	le := binary.LittleEndian
	le.PutUint32(buf[0:4], sparseMagic)
	le.PutUint32(buf[4:8], 1)  // version
	le.PutUint32(buf[8:12], 3) // flags: valid newline + redundant; uncompressed
	le.PutUint64(buf[12:20], uint64(totalSectors))
	le.PutUint64(buf[20:28], uint64(grainSectors))
	le.PutUint32(buf[44:48], numGTEsPerGT)
	le.PutUint64(buf[56:64], gdSector) // gdOffset
	le.PutUint64(buf[64:72], dataSector)

	// Grain directory: single entry pointing at the grain table.
	le.PutUint32(buf[gdSector*sectorSize:gdSector*sectorSize+4], gtSector)

	// Grain table: map each grain to a data sector, or 0 when its guest bytes
	// are all zero.
	for g := int64(0); g < gtes; g++ {
		start := g * grainBytes
		end := start + grainBytes
		if end > int64(len(guest)) {
			end = int64(len(guest))
		}
		sector := uint32(dataSector) + uint32(g*grainSectors)
		if allZero(guest[start:end]) {
			sector = 1 // zero grain
		}
		le.PutUint32(buf[gtSector*sectorSize+g*4:gtSector*sectorSize+g*4+4], sector)
	}

	// Data grains: copy guest bytes into their sectors.
	for g := int64(0); g < gtes; g++ {
		start := g * grainBytes
		end := start + grainBytes
		if end > int64(len(guest)) {
			end = int64(len(guest))
		}
		if allZero(guest[start:end]) {
			continue
		}
		dst := (dataSector + g*grainSectors) * sectorSize
		copy(buf[dst:], guest[start:end])
	}
	return buf
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func TestDiskReadsMonolithicSparse(t *testing.T) {
	guest := []byte("VMDK sparse test data")
	raw := writeMonolithicSparse(t, guest, 1)

	d, err := Open(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got := make([]byte, len(guest))
	if n, err := d.ReadAt(got, 0); err != nil || n != len(guest) {
		t.Fatalf("ReadAt: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, guest) {
		t.Fatalf("got %q, want %q", got, guest)
	}
}

func TestDiskReadsZeroAndSparseGrains(t *testing.T) {
	// 3 grains: data, zero, data. Middle grain reads back as zeroes.
	guest := make([]byte, 3*sectorSize)
	copy(guest[0:sectorSize], bytes.Repeat([]byte{0xAB}, sectorSize))
	copy(guest[2*sectorSize:3*sectorSize], bytes.Repeat([]byte{0xCD}, sectorSize))

	raw := writeMonolithicSparse(t, guest, 1)
	d, err := Open(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got := make([]byte, len(guest))
	if _, err := d.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, guest) {
		t.Fatalf("sparse read mismatch")
	}
}

func TestOpenRejectsNonVmdk(t *testing.T) {
	if _, err := Open(bytes.NewReader(make([]byte, 512))); err == nil {
		t.Fatal("expected non-vmdk data to be rejected")
	}
}
