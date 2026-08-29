package parallels

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestDiskReadsGuestExt(t *testing.T) {
	// tracks=2 ("WithouFreSpacExt"), two populated clusters and one sparse.
	guest := make([]byte, 3*1024)
	for i := range guest[:1024] {
		guest[i] = 0xAA
	}
	for i := range guest[2*1024 : 2*1024+512] {
		guest[2*1024+i] = 0xBB
	}
	raw := writeParallels(t, guest, 2, magicExt)

	d, err := Open(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if d.Size() != int64(len(guest)) {
		t.Fatalf("size %d, want %d", d.Size(), len(guest))
	}
	got := make([]byte, len(guest))
	if _, err := d.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, guest) {
		t.Fatalf("parallels read mismatch")
	}
}

func TestDiskReadsGuestPlain(t *testing.T) {
	// tracks=1 ("WithoutFreeSpace"), offMult=1.
	guest := []byte("parallels plain format data")
	raw := writeParallels(t, guest, 1, magicPlain)

	d, err := Open(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got := make([]byte, len(guest))
	if _, err := d.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, guest) {
		t.Fatalf("parallels read mismatch")
	}
}

func TestOpenRejectsNonParallels(t *testing.T) {
	if _, err := Open(bytes.NewReader(make([]byte, 64))); err == nil {
		t.Fatal("expected non-parallels data to be rejected")
	}
}

// writeParallels builds a minimal parallels image holding guest, using the
// given tracks (sectors per cluster) and magic.
func writeParallels(t *testing.T, guest []byte, tracks uint32, magic string) []byte {
	t.Helper()
	clusterSize := int64(tracks) * sectorSize
	nclusters := (int64(len(guest)) + clusterSize - 1) / clusterSize

	// Data area starts at a fixed sector, aligned to a cluster.
	const dataOff = 8
	if int64(dataOff)%int64(tracks) != 0 {
		t.Fatalf("dataOff %d not cluster-aligned to tracks %d", dataOff, tracks)
	}

	buf := make([]byte, dataOff*sectorSize+nclusters*clusterSize)
	le := binary.LittleEndian
	copy(buf[0:16], magic)
	le.PutUint32(buf[16:20], headerVer)
	le.PutUint32(buf[28:32], tracks)
	le.PutUint32(buf[32:36], uint32(nclusters))
	le.PutUint64(buf[36:44], uint64((len(guest)+sectorSize-1)/sectorSize))
	le.PutUint32(buf[48:52], dataOff)

	offMult := tracks
	if magic == magicPlain {
		offMult = 1
	}

	for c := int64(0); c < nclusters; c++ {
		start := c * clusterSize
		end := start + clusterSize
		if end > int64(len(guest)) {
			end = int64(len(guest))
		}
		clusterGuest := guest[start:end]
		if allZero(clusterGuest) {
			continue // leave bat entry 0 (sparse)
		}
		bat := uint32(int64(dataOff)/int64(offMult) + c)
		le.PutUint32(buf[headerSize+c*4:headerSize+c*4+4], bat)
		hostSector := int64(bat) * int64(offMult)
		copy(buf[hostSector*sectorSize:], clusterGuest)
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
