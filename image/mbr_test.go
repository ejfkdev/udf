package image

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeMBRWithExtended builds a raw disk image with an MBR holding one primary
// Linux partition and one extended partition, the latter containing two logical
// partitions described by an EBR chain.
func writeMBRWithExtended(t *testing.T) string {
	t.Helper()

	const sector = 512
	const extStart = 3048 // sector where the extended partition begins
	disk := make([]byte, 6000*sector)

	mbr := disk[:512]
	putPart := func(idx int, typ byte, start, size uint32) {
		e := mbr[446+idx*16 : 446+idx*16+16]
		e[4] = typ
		binary.LittleEndian.PutUint32(e[8:12], start)
		binary.LittleEndian.PutUint32(e[12:16], size)
	}
	putPart(0, 0x83, 2048, 1000)     // primary Linux
	putPart(1, 0x05, extStart, 2000) // extended container
	mbr[510], mbr[511] = 0x55, 0xaa

	// EBR 1 at the extended partition start: logical L1 + next EBR pointer.
	ebr1 := disk[extStart*sector : extStart*sector+512]
	l1 := ebr1[446:462]
	l1[4] = 0x83
	binary.LittleEndian.PutUint32(l1[8:12], 1)    // start relative to this EBR
	binary.LittleEndian.PutUint32(l1[12:16], 500) // 500 sectors
	next := ebr1[462:478]
	next[4] = 0x05
	binary.LittleEndian.PutUint32(next[8:12], 1000) // next EBR, relative to extStart
	ebr1[510], ebr1[511] = 0x55, 0xaa

	// EBR 2 at extStart+1000: logical L2, no successor.
	ebr2 := disk[(extStart+1000)*sector : (extStart+1000)*sector+512]
	l2 := ebr2[446:462]
	l2[4] = 0x83
	binary.LittleEndian.PutUint32(l2[8:12], 1)    // start relative to this EBR
	binary.LittleEndian.PutUint32(l2[12:16], 600) // 600 sectors
	ebr2[510], ebr2[511] = 0x55, 0xaa

	p := filepath.Join(t.TempDir(), "extended.raw")
	if err := os.WriteFile(p, disk, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func TestWalkEBR(t *testing.T) {
	disk := writeMBRWithExtended(t)
	data, err := os.ReadFile(disk)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	r := bytes.NewReader(data)

	ext := mbrExtendedStarts(r)
	if len(ext) != 1 || !ext[3048*512] {
		t.Fatalf("expected one extended partition at %d, got %v", 3048*512, ext)
	}

	lps := walkEBR(r, 3048*512)
	if len(lps) != 2 {
		t.Fatalf("expected 2 logical partitions, got %d: %+v", len(lps), lps)
	}
	if lps[0].start != 3049*512 || lps[0].size != 500*512 {
		t.Fatalf("unexpected first logical partition: %+v", lps[0])
	}
	if lps[1].start != 4049*512 || lps[1].size != 600*512 {
		t.Fatalf("unexpected second logical partition: %+v", lps[1])
	}
}

func TestDiscoverLogicalPartitions(t *testing.T) {
	path := writeMBRWithExtended(t)

	tmp := filepath.Join(t.TempDir(), "copy.raw")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	entries, err := ListDisk(tmp, "/")
	if err != nil {
		t.Fatalf("list disk: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	// The extended container (p2) must not appear; logical partitions are p5/p6.
	if names["p2"] || names["p3"] || names["p4"] {
		t.Fatalf("extended container leaked into listing: %v", names)
	}
	if !names["p5"] || !names["p6"] {
		t.Fatalf("expected logical partitions p5 and p6, got %v", names)
	}
}
