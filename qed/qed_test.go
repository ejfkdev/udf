package qed

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func putU32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
func putU64(b []byte, off int, v uint64) { binary.LittleEndian.PutUint64(b[off:], v) }

// buildQED constructs a minimal in-memory QED image with cluster_size=4096,
// table_size=1 and a known layout:
//
//	cluster 0            header
//	cluster 1            L1 table
//	cluster 2            L2 table #0
//	clusters 3,4,5       data clusters for guest clusters 0,1,2
//	guest cluster 3      zero cluster (L2 entry = 1)
//	guest clusters >=4   unallocated (L2 entry = 0)
func buildQED(t *testing.T) io.ReaderAt {
	t.Helper()
	const (
		clusterSize = 4096
		tableSize   = 1
		headerSize  = 1
		l1Offset    = 1 * clusterSize
		l2Offset    = 2 * clusterSize
		dataBase    = 3 * clusterSize
		imageSize   = 8 * clusterSize
	)

	buf := make([]byte, dataBase+3*clusterSize)
	putU32(buf, 0, qedMagic)
	putU32(buf, 4, clusterSize)
	putU32(buf, 8, tableSize)
	putU32(buf, 12, headerSize)
	putU64(buf, 40, uint64(l1Offset))
	putU64(buf, 48, uint64(imageSize))

	putU64(buf, l1Offset+0*8, uint64(l2Offset))
	putU64(buf, l2Offset+0*8, uint64(dataBase+0*clusterSize))
	putU64(buf, l2Offset+1*8, uint64(dataBase+1*clusterSize))
	putU64(buf, l2Offset+2*8, uint64(dataBase+2*clusterSize))
	putU64(buf, l2Offset+3*8, 1) // zero cluster

	for i := 0; i < 3; i++ {
		for j := range clusterSize {
			buf[dataBase+i*clusterSize+j] = byte((i + j) & 0xff)
		}
	}
	return bytes.NewReader(buf)
}

// pattern reproduces the fixture's fill: data cluster c, byte j within the
// cluster, holds (c + j) & 0xff.
func pattern(t *testing.T, start, n int) []byte {
	t.Helper()
	const clusterSize = 4096
	out := make([]byte, n)
	for j := 0; j < n; j++ {
		off := start + j
		out[j] = byte((off/clusterSize + off%clusterSize) & 0xff)
	}
	return out
}

func TestDiskReadsAllocatedData(t *testing.T) {
	d, err := Open(buildQED(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if d.Size() != 8*4096 {
		t.Fatalf("Size = %d, want %d", d.Size(), 8*4096)
	}

	// Three contiguous allocated clusters.
	got := make([]byte, 3*4096)
	if _, err := d.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, pattern(t, 0, 3*4096)) {
		t.Fatalf("allocated data mismatch")
	}
}

func TestDiskReadsUnalignedAcrossClusterBoundary(t *testing.T) {
	d, _ := Open(buildQED(t))
	got := make([]byte, 3500)
	if _, err := d.ReadAt(got, 1000); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, pattern(t, 1000, 3500)) {
		t.Fatalf("unaligned read mismatch")
	}
}

func TestDiskReadsZeroCluster(t *testing.T) {
	d, _ := Open(buildQED(t))
	got := make([]byte, 4096)
	if _, err := d.ReadAt(got, 3*4096); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, make([]byte, 4096)) {
		t.Fatalf("zero cluster did not return zeros")
	}
}

func TestDiskReadsUnallocatedCluster(t *testing.T) {
	d, _ := Open(buildQED(t))
	got := make([]byte, 4096)
	if _, err := d.ReadAt(got, 4*4096); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, make([]byte, 4096)) {
		t.Fatalf("unallocated cluster did not return zeros")
	}
}

func TestDiskReadPastEndClamps(t *testing.T) {
	d, _ := Open(buildQED(t))
	got := make([]byte, 500)
	n, err := d.ReadAt(got, d.Size()-100)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 100 {
		t.Fatalf("n = %d, want 100", n)
	}
}

func TestDiskReadAtEndReturnsEOF(t *testing.T) {
	d, _ := Open(buildQED(t))
	if _, err := d.ReadAt(make([]byte, 10), d.Size()); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestOpenRejectsNonQED(t *testing.T) {
	if _, err := Open(bytes.NewReader(make([]byte, 512))); err == nil {
		t.Fatalf("Open accepted a non-QED image")
	}
}
