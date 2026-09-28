package image

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures here build an Android super image (dynamic partitions) around a
// real ext4 filesystem: once as a single extent, once split across two
// non-adjacent extents. The fragmented case is the point — the filesystem has
// to be read through an extent mapping, not as a byte range — so the layout is
// written by hand rather than by the reader it tests.

const superTestSector = 512

type superTestExtent struct {
	sectors uint64
	target  uint32 // 0 linear, 1 zero
	data    uint64
}

type superTestPart struct {
	name    string
	extents []superTestExtent
}

// superTestImage lays out metadata and partition data.
func superTestImage(t *testing.T, size int64, parts []superTestPart, blobs [][]byte) string {
	t.Helper()
	img := make([]byte, size)

	const (
		geomMagic   = 0x616c4467
		headerMagic = 0x414c5030
		slotSize    = 65536
	)
	geom := make([]byte, 52)
	binary.LittleEndian.PutUint32(geom[0:4], geomMagic)
	binary.LittleEndian.PutUint32(geom[4:8], 52)
	binary.LittleEndian.PutUint32(geom[40:44], slotSize)
	binary.LittleEndian.PutUint32(geom[44:48], 2)
	binary.LittleEndian.PutUint32(geom[48:52], 4096)
	copy(img[4096:], geom)
	copy(img[8192:], geom)

	// Data blobs go after the metadata area; each partition names their sectors.
	const dataStart = 1 << 20
	offsets := make([]uint64, len(blobs))
	next := uint64(dataStart)
	for i, b := range blobs {
		offsets[i] = next
		copy(img[next:], b)
		next += uint64(len(b))
		next = (next + 4096 - 1) &^ 4095 // keep the regions far apart
	}

	var extTable []byte
	var firstExt []uint32
	for _, p := range parts {
		firstExt = append(firstExt, uint32(len(extTable)/24))
		for _, e := range p.extents {
			buf := make([]byte, 24)
			binary.LittleEndian.PutUint64(buf[0:8], e.sectors)
			binary.LittleEndian.PutUint32(buf[8:12], e.target)
			binary.LittleEndian.PutUint64(buf[12:20], e.data)
			extTable = append(extTable, buf...)
		}
	}
	var partTable []byte
	for i, p := range parts {
		buf := make([]byte, 52)
		copy(buf[:36], p.name)
		binary.LittleEndian.PutUint32(buf[40:44], firstExt[i])
		binary.LittleEndian.PutUint32(buf[44:48], uint32(len(p.extents)))
		partTable = append(partTable, buf...)
	}

	header := make([]byte, 128)
	binary.LittleEndian.PutUint32(header[0:4], headerMagic)
	binary.LittleEndian.PutUint16(header[4:6], 10)
	binary.LittleEndian.PutUint32(header[8:12], 128)
	binary.LittleEndian.PutUint32(header[44:48], uint32(len(partTable)+len(extTable)))
	binary.LittleEndian.PutUint32(header[80:84], 0)
	binary.LittleEndian.PutUint32(header[84:88], uint32(len(parts)))
	binary.LittleEndian.PutUint32(header[88:92], 52)
	binary.LittleEndian.PutUint32(header[92:96], uint32(len(partTable)))
	binary.LittleEndian.PutUint32(header[96:100], uint32(len(extTable)/24))
	binary.LittleEndian.PutUint32(header[100:104], 24)

	for i := 0; i < 2; i++ {
		base := 4096 + 2*4096 + i*slotSize
		copy(img[base:], header)
		copy(img[base+128:], partTable)
		copy(img[base+128+len(partTable):], extTable)
	}

	path := filepath.Join(t.TempDir(), "super.img")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = offsets
	return path
}

func TestDiskSuperPartitions(t *testing.T) {
	// A real ext4 filesystem, from the same builder the other disk tests use.
	ext4Path := createExt4Image(t)
	fs, err := os.ReadFile(ext4Path)
	if err != nil {
		t.Fatal(err)
	}

	// Split it at a sector boundary that is not a block boundary, so the
	// pieces are not a valid filesystem on their own.
	split := int64(len(fs)/2) &^ 4095
	first, second := fs[:split], fs[split:]

	// One partition whole, one fragmented across two extents placed in the
	// opposite order (so the second extent is *before* the first in the file).
	var blobs [][]byte
	blobs = append(blobs, fs, second, first)
	const dataStart = 1 << 20
	wholeSector := uint64(dataStart) / superTestSector
	secondSector := wholeSector + uint64(len(fs)+4095)/4096*8
	firstSector := secondSector + uint64(len(second)+4095)/4096*8
	if firstSector < secondSector {
		t.Fatal("fixture layout broken")
	}

	parts := []superTestPart{
		{name: "system", extents: []superTestExtent{
			{sectors: uint64((len(fs) + 511) / 512), target: 0, data: wholeSector},
		}},
		{name: "vendor", extents: []superTestExtent{
			{sectors: uint64((len(first) + 511) / 512), target: 0, data: firstSector},
			{sectors: uint64((len(second) + 511) / 512), target: 0, data: secondSector},
		}},
	}
	path := superTestImage(t, int64(len(fs))*2+8<<20, parts, blobs)

	// The fragmented volume must be detected as ext4 through its mapping.
	meta, err := ScanDiskMetadata(path)
	if err != nil {
		t.Fatalf("ScanDiskMetadata: %v", err)
	}
	if len(meta.Disks) != 1 {
		t.Fatalf("got %d disks", len(meta.Disks))
	}
	kinds := map[string]DiskVolume{}
	for _, v := range meta.Disks[0].Volumes {
		kinds[v.Name] = v
	}
	for _, name := range []string{"system", "vendor"} {
		v, ok := kinds[name]
		if !ok {
			t.Fatalf("volume %s missing from %v", name, meta.Disks[0].Volumes)
		}
		if v.Kind != "super" {
			t.Errorf("volume %s kind = %q, want super", name, v.Kind)
		}
		if v.FSType != "ext4" {
			t.Errorf("volume %s filesystem = %q, want ext4", name, v.FSType)
		}
		if v.Size != int64(len(fs)) {
			t.Errorf("volume %s size = %d, want %d", name, v.Size, len(fs))
		}
	}

	// Both must list and extract their files: the fragmented one proves reads
	// stitch across extents.
	for _, vol := range []string{"system", "vendor"} {
		entries, err := ListDisk(path, vol)
		if err != nil {
			t.Fatalf("ListDisk(%s): %v", vol, err)
		}
		found := false
		for _, e := range entries {
			if e.Name == "etc" {
				found = true
			}
		}
		if !found {
			t.Fatalf("ListDisk(%s) has no etc directory: %v", vol, entries)
		}
		dest := filepath.Join(t.TempDir(), vol+"-passwd")
		if _, err := ExtractDiskPath(path, vol+"/etc/passwd", dest, 1<<20); err != nil {
			t.Fatalf("ExtractDiskPath(%s/etc/passwd): %v", vol, err)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, []byte("root:x:0:0\n")) {
			t.Fatalf("%s/etc/passwd = %q", vol, got)
		}
	}
}
