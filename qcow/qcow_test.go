package qcow

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"testing"
)

// buildQCOW constructs an in-memory QCOW v1 image with cluster_size=4096,
// l2_bits=2 and 8 guest clusters:
//
//	clusters 0,1      allocated
//	cluster  2        compressed (zlib)
//	cluster  3        unallocated
//	clusters 4,5      allocated
//	clusters 6,7      unallocated
//
// Every allocated/compressed cluster c, byte j, holds (c + j) & 0xff.
func buildQCOW(t *testing.T) io.ReaderAt {
	t.Helper()
	const (
		clusterSize = 4096
		clusterBits = 12
		l2Bits      = 2
	)
	diskSize := int64(8 * clusterSize)

	l1Off := int64(4096)
	l2Base := int64(8192)
	dataBase := int64(16384)
	compressedOff := dataBase + 8*clusterSize

	content := func(c int) []byte {
		out := make([]byte, clusterSize)
		for j := 0; j < clusterSize; j++ {
			out[j] = byte((c + j) & 0xff)
		}
		return out
	}

	var comp bytes.Buffer
	zw := zlib.NewWriter(&comp)
	if _, err := zw.Write(content(2)); err != nil {
		t.Fatal(err)
	}
	zw.Close()

	buf := make([]byte, compressedOff+clusterSize)
	be := binary.BigEndian
	be.PutUint32(buf[0:4], qcowMagic)
	be.PutUint32(buf[4:8], qcowVersion)
	be.PutUint64(buf[24:32], uint64(diskSize))
	buf[32] = clusterBits
	buf[33] = l2Bits
	be.PutUint32(buf[36:40], qcowCryptNone)
	be.PutUint64(buf[40:48], uint64(l1Off))

	be.PutUint64(buf[l1Off:l1Off+8], uint64(l2Base))
	be.PutUint64(buf[l1Off+8:l1Off+16], uint64(l2Base+4096))

	putL2 := func(base int64, cluster int, entry uint64) {
		be.PutUint64(buf[base+int64(cluster)*8:base+int64(cluster)*8+8], entry)
	}
	// L2 table 0: guest clusters 0..3.
	putL2(l2Base, 0, uint64(dataBase+0*clusterSize))
	putL2(l2Base, 1, uint64(dataBase+1*clusterSize))
	mask := uint64(1)<<(63-clusterBits) - 1
	compEntry := qcowOflagCompressed |
		(uint64(comp.Len())&uint64(clusterSize-1))<<(63-clusterBits) |
		(uint64(compressedOff) & mask)
	putL2(l2Base, 2, compEntry)
	putL2(l2Base, 3, 0) // unallocated
	// L2 table 1: guest clusters 4..7.
	putL2(l2Base+4096, 0, uint64(dataBase+4*clusterSize))
	putL2(l2Base+4096, 1, uint64(dataBase+5*clusterSize))
	putL2(l2Base+4096, 2, 0)
	putL2(l2Base+4096, 3, 0)

	for _, c := range []int{0, 1, 4, 5} {
		host := dataBase + int64(c)*clusterSize
		copy(buf[host:host+clusterSize], content(c))
	}
	copy(buf[compressedOff:compressedOff+int64(comp.Len())], comp.Bytes())

	return bytes.NewReader(buf)
}

func pattern(start, n int) []byte {
	const clusterSize = 4096
	out := make([]byte, n)
	for j := 0; j < n; j++ {
		off := start + j
		out[j] = byte((off/clusterSize + off%clusterSize) & 0xff)
	}
	return out
}

func TestReadsAllocatedData(t *testing.T) {
	d, err := Open(buildQCOW(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if d.Size() != 8*4096 {
		t.Fatalf("Size = %d, want %d", d.Size(), 8*4096)
	}
	got := make([]byte, 2*4096)
	if _, err := d.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, pattern(0, 2*4096)) {
		t.Fatalf("allocated data mismatch")
	}
}

func TestReadsCompressedCluster(t *testing.T) {
	d, _ := Open(buildQCOW(t))
	got := make([]byte, 4096)
	if _, err := d.ReadAt(got, 2*4096); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, pattern(2*4096, 4096)) {
		t.Fatalf("compressed cluster mismatch")
	}
}

func TestReadsUnallocatedCluster(t *testing.T) {
	d, _ := Open(buildQCOW(t))
	got := make([]byte, 4096)
	if _, err := d.ReadAt(got, 3*4096); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, make([]byte, 4096)) {
		t.Fatalf("unallocated cluster did not return zeros")
	}
}

func TestReadsAcrossClusterTypes(t *testing.T) {
	d, _ := Open(buildQCOW(t))
	got := make([]byte, 7500)
	if _, err := d.ReadAt(got, 1000); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, pattern(1000, 7500)) {
		t.Fatalf("cross-cluster read mismatch")
	}
}

func TestReadPastEndClamps(t *testing.T) {
	d, _ := Open(buildQCOW(t))
	got := make([]byte, 500)
	n, err := d.ReadAt(got, d.Size()-100)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 100 {
		t.Fatalf("n = %d, want 100", n)
	}
}

func TestReadAtEndReturnsEOF(t *testing.T) {
	d, _ := Open(buildQCOW(t))
	if _, err := d.ReadAt(make([]byte, 10), d.Size()); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestOpenRejectsNonQcow(t *testing.T) {
	if _, err := Open(bytes.NewReader(make([]byte, 48))); err == nil {
		t.Fatalf("Open accepted a non-QCOW image")
	}
}

func TestOpenRejectsBackingFile(t *testing.T) {
	raw := buildQCOW(t)
	buf := make([]byte, 48)
	if _, err := raw.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(buf[16:20], 32) // backing_file_size != 0
	if _, err := Open(bytes.NewReader(buf)); err == nil {
		t.Fatalf("Open accepted an image with a backing file")
	}
}
