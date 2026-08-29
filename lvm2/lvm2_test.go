package lvm2

import (
	"bytes"
	"testing"
)

func testMetadata(seqno string, extraLVs string) string {
	return `contents = "Text Format Volume Group"
version = 1

vg1 {
id = "yPpVTp-MPp2-FwJ0-Nk0C-SWkN-vlL5-ThX4SH"
seqno = ` + seqno + `
format = "lvm2"
status = ["RESIZEABLE", "READ", "WRITE"]
flags = []
extent_size = 8192
max_lv = 0
max_pv = 0
metadata_copies = 0

physical_volumes {

pv0 {
id = "UEYEKK-bjaq-0ihs-iooU-gqpj-1eaA-89bhMU"
device = "/dev/vda3"

status = ["ALLOCATABLE"]
flags = []
dev_size = 250343424
pe_start = 2048
pe_count = 30559
}
}

logical_volumes {

root {
segment_count = 1

segment1 {
start_extent = 0
extent_count = 1024

type = "striped"
stripe_count = 1

stripes = [
"pv0", 0
]
}
}
` + extraLVs + `
}

}
`
}

func TestParseMetadataBasic(t *testing.T) {
	vg, err := parseMetadata(testMetadata("3", ""))
	if err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	if vg.Name != "vg1" || vg.Seqno != 3 {
		t.Fatalf("unexpected vg: %+v", vg)
	}
	if vg.ExtentBytes != 8192*512 || vg.PEStart != 2048*512 || vg.PECount != 30559 {
		t.Fatalf("unexpected vg geometry: %+v", vg)
	}
	if len(vg.Volumes) != 1 || vg.Volumes[0].Name != "root" {
		t.Fatalf("unexpected volumes: %+v", vg.Volumes)
	}

	lv := vg.Volumes[0]
	if lv.Size != 1024*4096*1024 {
		t.Fatalf("unexpected lv size: %d", lv.Size)
	}
	if len(lv.Extents) != 1 {
		t.Fatalf("expected one contiguous extent, got %d", len(lv.Extents))
	}
	if lv.Extents[0].Start != 2048*512 || lv.Extents[0].Size != 1024*4096*1024 {
		t.Fatalf("unexpected extent: %+v", lv.Extents[0])
	}
}

func TestParseMetadataMultipleVolumes(t *testing.T) {
	extra := `
data {
segment_count = 1

segment1 {
start_extent = 0
extent_count = 2048

type = "striped"
stripe_count = 1

stripes = [
"pv0", 1024
]
}
}
`
	vg, err := parseMetadata(testMetadata("4", extra))
	if err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	if len(vg.Volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(vg.Volumes))
	}
	data := vg.Volumes[1]
	if data.Name != "data" {
		t.Fatalf("unexpected second volume: %q", data.Name)
	}
	// stripe offset 1024 extents past the first PE.
	wantStart := int64(2048*512) + 1024*(8192*512)
	if data.Extents[0].Start != wantStart {
		t.Fatalf("unexpected data start: %d want %d", data.Extents[0].Start, wantStart)
	}
}

func TestOpenPicksHighestSeqno(t *testing.T) {
	old := testMetadata("2", "")
	latest := testMetadata("9", `
data {
segment_count = 1

segment1 {
start_extent = 0
extent_count = 2048

type = "striped"
stripe_count = 1

stripes = [
"pv0", 1024
]
}
}
`)
	buf := makeTestPV(t, old+"\x00"+latest)

	vg, err := Open(bytes.NewReader(buf), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if vg.Seqno != 9 {
		t.Fatalf("expected newest seqno 9, got %d", vg.Seqno)
	}
	if len(vg.Volumes) != 2 {
		t.Fatalf("expected 2 volumes from newest snapshot, got %d", len(vg.Volumes))
	}
}

func TestOpenRejectsNonLVM(t *testing.T) {
	buf := make([]byte, sectorSize*4)
	if _, err := Open(bytes.NewReader(buf), 0); err == nil {
		t.Fatal("expected non-LVM data to be rejected")
	}
}

// makeTestPV builds a byte buffer mimicking the head of a PV: a zero-filled
// first sector, an LVM2 label at sector 1, and the metadata text at a fixed
// offset, all below the 8 MiB scan window.
func makeTestPV(t *testing.T, text string) []byte {
	t.Helper()
	buf := make([]byte, 8<<20)
	copy(buf[sectorSize:sectorSize+8], "LABELONE")
	copy(buf[sectorSize+24:sectorSize+32], "LVM2 001")

	// Place the text at sector 32 (the conventional metadata area start).
	offset := 32 * sectorSize
	if len(text) > len(buf)-offset {
		t.Fatalf("metadata text too large")
	}
	// LVM pads metadata with NULs; terminator is not significant.
	for i, c := range []byte(text) {
		buf[offset+i] = c
	}
	return buf
}
