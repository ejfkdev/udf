package superlp

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures below are hand-written LP metadata: geometry, two metadata
// slots, and the partition/extent tables liblp lays out. They are written
// independently of the reader (and cross-checked against a real emulator
// system.img in the image package's tests).

type ext struct {
	sectors uint64
	target  uint32
	data    uint64
	source  uint32
}

type part struct {
	name    string
	extents []ext
}

const (
	testSectorSize = 512
	slotSize       = 65536
)

// superWriter lays out a super image: metadata first, then partition data at
// the sector offsets the extents name.
type superWriter struct {
	img    []byte
	extent []ext
	parts  []part
}

func newSuperWriter(size int) *superWriter {
	return &superWriter{img: make([]byte, size)}
}

// putData writes data at a sector offset and returns that offset.
func (w *superWriter) putData(sector uint64, data []byte) ext {
	off := int(sector * testSectorSize)
	copy(w.img[off:], data)
	return ext{sectors: uint64((len(data) + testSectorSize - 1) / testSectorSize), target: TargetLinear, data: sector}
}

func (w *superWriter) build() []byte {
	geom := make([]byte, geometryStructSize)
	binary.LittleEndian.PutUint32(geom[0:4], GeometryMagic)
	binary.LittleEndian.PutUint32(geom[4:8], geometryStructSize)
	binary.LittleEndian.PutUint32(geom[40:44], slotSize)
	binary.LittleEndian.PutUint32(geom[44:48], 2)
	binary.LittleEndian.PutUint32(geom[48:52], 4096)
	copy(w.img[GeometryOffset:], geom)
	copy(w.img[GeometryOffset+GeometrySize:], geom) // the second copy

	// One extents table, one partitions table.
	partsBytes := make([]byte, 0, len(w.parts)*52)
	firstExt := make([]uint32, len(w.parts))
	for i, p := range w.parts {
		firstExt[i] = uint32(len(w.extent))
		for _, e := range p.extents {
			w.extent = append(w.extent, e)
		}
		buf := make([]byte, 52)
		copy(buf[:36], p.name)
		binary.LittleEndian.PutUint32(buf[40:44], firstExt[i])
		binary.LittleEndian.PutUint32(buf[44:48], uint32(len(p.extents)))
		partsBytes = append(partsBytes, buf...)
	}
	extBytes := make([]byte, 0, len(w.extent)*24)
	for _, e := range w.extent {
		buf := make([]byte, 24)
		binary.LittleEndian.PutUint64(buf[0:8], e.sectors)
		binary.LittleEndian.PutUint32(buf[8:12], e.target)
		binary.LittleEndian.PutUint64(buf[12:20], e.data)
		binary.LittleEndian.PutUint32(buf[20:24], e.source)
		extBytes = append(extBytes, buf...)
	}

	header := make([]byte, metadataHeaderSize)
	binary.LittleEndian.PutUint32(header[0:4], HeaderMagic)
	binary.LittleEndian.PutUint16(header[4:6], 10)
	binary.LittleEndian.PutUint16(header[6:8], 0)
	binary.LittleEndian.PutUint32(header[8:12], metadataHeaderSize)
	tablesSize := len(partsBytes) + len(extBytes)
	binary.LittleEndian.PutUint32(header[44:48], uint32(tablesSize))
	// Descriptors: partitions, extents, groups, block devices. Offsets are
	// relative to the tables area, which starts after the header.
	binary.LittleEndian.PutUint32(header[80:84], 0)
	binary.LittleEndian.PutUint32(header[84:88], uint32(len(w.parts)))
	binary.LittleEndian.PutUint32(header[88:92], 52)
	binary.LittleEndian.PutUint32(header[92:96], uint32(len(partsBytes)))
	binary.LittleEndian.PutUint32(header[96:100], uint32(len(w.extent)))
	binary.LittleEndian.PutUint32(header[100:104], 24)

	slot := GeometryOffset + 2*GeometrySize
	for i := 0; i < 2; i++ {
		base := slot + i*slotSize
		copy(w.img[base:], header)
		copy(w.img[base+metadataHeaderSize:], partsBytes)
		copy(w.img[base+metadataHeaderSize+len(partsBytes):], extBytes)
	}
	return w.img
}

func writeSuperFile(t *testing.T, w *superWriter) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "super.img")
	if err := os.WriteFile(path, w.build(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSuperOpenAndMap(t *testing.T) {
	system := bytes.Repeat([]byte("system-data-"), 4096) // 48 KiB
	vendorA := bytes.Repeat([]byte("vendor-part-a"), 1024)
	vendorB := bytes.Repeat([]byte("vendor-part-b"), 2048)

	w := newSuperWriter(8 << 20)
	e1 := w.putData(2048, system)
	e2a := w.putData(4096, vendorA)
	e2b := w.putData(9000, vendorB) // deliberately not adjacent to e2a
	w.parts = []part{
		{name: "system", extents: []ext{e1}},
		{name: "vendor", extents: []ext{e2a, e2b}},
		{name: "blank", extents: []ext{{sectors: 8, target: TargetZero}}},
		{name: "mixed", extents: []ext{{sectors: 4, target: TargetLinear, data: 2048}, {sectors: 4, target: TargetZero}}},
	}
	path := writeSuperFile(t, w)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if !Detect(f, 0) {
		t.Fatal("Detect said no on a super image")
	}
	md, err := Open(f, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(md.Partitions) != 4 {
		t.Fatalf("got %d partitions, want 4", len(md.Partitions))
	}

	sys, ok := md.Find("system")
	if !ok {
		t.Fatal("no system partition")
	}
	if sys.Size() != int64(len(system)) {
		t.Fatalf("system size = %d, want %d", sys.Size(), len(system))
	}
	if _, _, contig := sys.Contiguous(); !contig {
		t.Fatal("single-extent partition should be contiguous")
	}
	m, err := md.Mapping(f, 0, sys)
	if err != nil {
		t.Fatalf("Mapping(system): %v", err)
	}
	got := make([]byte, m.Size())
	if _, err := m.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatalf("read system: %v", err)
	}
	if !bytes.Equal(got, system) {
		t.Fatal("system bytes do not round-trip")
	}

	vendor, _ := md.Find("vendor")
	if _, _, contig := vendor.Contiguous(); contig {
		t.Fatal("two non-adjacent extents are not contiguous")
	}
	vm, err := md.Mapping(f, 0, vendor)
	if err != nil {
		t.Fatalf("Mapping(vendor): %v", err)
	}
	want := append(append([]byte{}, vendorA...), vendorB...)
	gotV := make([]byte, vm.Size())
	if _, err := vm.ReadAt(gotV, 0); err != nil && err != io.EOF {
		t.Fatalf("read vendor: %v", err)
	}
	if !bytes.Equal(gotV, want) {
		t.Fatal("fragmented partition bytes do not stitch across extents")
	}
	// A read that straddles the fragmentation boundary.
	bridge := make([]byte, 32)
	if _, err := vm.ReadAt(bridge, int64(len(vendorA)-16)); err != nil && err != io.EOF {
		t.Fatalf("straddling read: %v", err)
	}
	if !bytes.Equal(bridge, want[len(vendorA)-16:len(vendorA)+16]) {
		t.Fatal("straddling read returned the wrong bytes")
	}

	blank, _ := md.Find("blank")
	bm, err := md.Mapping(f, 0, blank)
	if err != nil {
		t.Fatalf("Mapping(blank): %v", err)
	}
	zeros := make([]byte, bm.Size())
	for i := range zeros {
		zeros[i] = 0xff
	}
	if _, err := bm.ReadAt(zeros, 0); err != nil && err != io.EOF {
		t.Fatalf("read blank: %v", err)
	}
	if !bytes.Equal(zeros, make([]byte, bm.Size())) {
		t.Fatal("a zero extent should read as zeroes")
	}

	mixed, _ := md.Find("mixed")
	mm, err := md.Mapping(f, 0, mixed)
	if err != nil {
		t.Fatalf("Mapping(mixed): %v", err)
	}
	gotM := make([]byte, mm.Size())
	if _, err := mm.ReadAt(gotM, 0); err != nil && err != io.EOF {
		t.Fatalf("read mixed: %v", err)
	}
	if !bytes.Equal(gotM[:2048], system[:2048]) || !bytes.Equal(gotM[2048:], make([]byte, 2048)) {
		t.Fatal("mixed partition should be data then zeroes")
	}

	// Reading past the end reports EOF, and a short read at the tail works.
	if _, err := mm.ReadAt(make([]byte, 16), mm.Size()); err != io.EOF {
		t.Fatalf("read past the end = %v, want EOF", err)
	}
}

func TestSuperSlotFallbackAndErrors(t *testing.T) {
	w := newSuperWriter(8 << 20)
	e := w.putData(2048, []byte("payload"))
	w.parts = []part{{name: "system", extents: []ext{e}}}
	img := w.build()

	// Corrupt the primary slot: Open must fall back to the second one.
	slot := GeometryOffset + 2*GeometrySize
	img[slot] = 0
	corrupt := filepath.Join(t.TempDir(), "corrupt.img")
	if err := os.WriteFile(corrupt, img, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := Open(f, 0); err != nil {
		t.Fatalf("Open with a corrupt primary slot: %v", err)
	}

	// No geometry at all is not a super.
	plain := make([]byte, 1<<16)
	plainPath := filepath.Join(t.TempDir(), "plain.img")
	if err := os.WriteFile(plainPath, plain, 0o644); err != nil {
		t.Fatal(err)
	}
	pf, err := os.Open(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	if Detect(pf, 0) {
		t.Fatal("Detect said yes on zeroes")
	}
	if _, err := Open(pf, 0); err == nil {
		t.Fatal("Open accepted a file with no geometry")
	}

	// An extent living on another block device cannot be mapped from here.
	md, err := Open(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	foreign := Partition{Name: "other", Extents: []Extent{{NumSectors: 1, TargetType: TargetLinear, TargetData: 0, TargetSource: 1}}}
	if _, err := md.Mapping(f, 0, foreign); err == nil {
		t.Fatal("Mapping accepted an extent on a foreign block device")
	}
}
