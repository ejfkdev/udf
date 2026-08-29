package vdi

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestDiskReadsBlocks(t *testing.T) {
	blockSize := uint32(4096)
	guest := make([]byte, 2*blockSize) // block 0 populated, block 1 sparse-zero
	for i := range guest[:blockSize] {
		guest[i] = byte(i%251 + 1)
	}
	raw := writeVDI(t, guest[0:blockSize], blockSize, 2)

	d, err := Open(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("open vdi: %v", err)
	}
	if d.Size() != int64(len(guest)) {
		t.Fatalf("unexpected size %d", d.Size())
	}
	got := make([]byte, len(guest))
	if _, err := d.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, guest) {
		t.Fatalf("vdi data mismatch")
	}
}

func TestOpenRejectsNonVdi(t *testing.T) {
	if _, err := Open(bytes.NewReader(make([]byte, 512))); err == nil {
		t.Fatal("expected non-vdi data to be rejected")
	}
}

// writeVDI builds a minimal VDI whose populated blocks hold `data` followed by
// one sparse-zero block, using the given block size and total block count.
func writeVDI(t *testing.T, data []byte, blockSize, numBlocks uint32) []byte {
	t.Helper()
	headerOffset := uint32(512)
	bmapOffset := headerOffset
	dataOffset := bmapOffset + numBlocks*4
	total := dataOffset + uint32(len(data))

	buf := make([]byte, total)
	le := binary.LittleEndian
	le.PutUint32(buf[0x40:0x44], vdiSignature)
	le.PutUint32(buf[0x4c:0x50], 1)            // dynamic
	le.PutUint32(buf[0x154:0x158], bmapOffset) // offset_bmap
	le.PutUint32(buf[0x158:0x15c], dataOffset) // offset_data
	le.PutUint64(buf[0x170:0x178], uint64(numBlocks)*uint64(blockSize))
	le.PutUint32(buf[0x178:0x17c], blockSize)
	le.PutUint32(buf[0x180:0x184], numBlocks)
	le.PutUint32(buf[0x184:0x188], 1) // blocks_allocated

	// bmap: block 0 -> data block 0, rest unallocated.
	le.PutUint32(buf[bmapOffset:bmapOffset+4], 0)
	for i := uint32(1); i < numBlocks; i++ {
		le.PutUint32(buf[bmapOffset+i*4:bmapOffset+i*4+4], unallocated)
	}
	copy(buf[dataOffset:], data)
	return buf
}
