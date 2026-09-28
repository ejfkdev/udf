// Package superlp reads Android "super" partitions: the LP metadata that
// describes dynamic partitions (system, vendor, product, …) as an extent list
// over one block device.
//
// A super partition starts with two copies of a geometry block, then the
// metadata slots (two by default, plus backups), then the partitions' data.
// Every logical partition is a list of extents — each either a range of the
// super partition or a run of zeroes — so reading one means stitching those
// ranges together, which is what Mapping does.
//
// The layout is the one liblp writes; Open validates it against the geometry
// rather than trusting the numbers, because a corrupt super should be reported
// as unreadable, not read as garbage.
package superlp

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	// GeometryMagic ("gDla") and HeaderMagic ("0PLA") are the two magics liblp
	// writes; the geometry sits at GeometryOffset and is written twice.
	GeometryMagic  = 0x616c4467
	HeaderMagic    = 0x414c5030
	GeometryOffset = 4096
	GeometrySize   = 4096
	SectorSize     = 512

	geometryStructSize = 52
	// A v10 metadata header is 80 fixed bytes plus four 12-byte table
	// descriptors; later versions append flags and reserved space, so the
	// header's own size field is what the tables area starts at.
	metadataHeaderSize = 128
	maxMetadataSize    = 16 << 20
	maxSlots           = 8
	maxEntries         = 1 << 20
	nameLen            = 36

	// Target types of an extent.
	TargetLinear = 0
	TargetZero   = 1
)

// Geometry is the metadata geometry block.
type Geometry struct {
	MaxMetadataSize uint32
	SlotCount       uint32
	LogicalBlockSz  uint32
}

// Extent is one range of a logical partition: NumSectors sectors starting at
// TargetData on block device TargetSource, or—when TargetType is TargetZero—
// NumSectors sectors that read as zeroes.
type Extent struct {
	NumSectors   uint64
	TargetType   uint32
	TargetData   uint64
	TargetSource uint32
}

// Partition is one logical partition: a name and the extents that hold it.
type Partition struct {
	Name       string
	Attributes uint32
	Extents    []Extent
}

// Size is the partition's size in bytes.
func (p Partition) Size() int64 {
	var sectors uint64
	for _, e := range p.Extents {
		sectors += e.NumSectors
	}
	return int64(sectors * SectorSize)
}

// Contiguous reports whether the partition is a single uninterrupted run of the
// super partition, and returns the run's byte offset and size when it is.
func (p Partition) Contiguous() (offset, size int64, ok bool) {
	if len(p.Extents) == 0 {
		return 0, 0, false
	}
	for i, e := range p.Extents {
		if e.TargetType != TargetLinear || e.TargetSource != 0 {
			return 0, 0, false
		}
		if i > 0 && p.Extents[i-1].TargetData+p.Extents[i-1].NumSectors != e.TargetData {
			return 0, 0, false
		}
	}
	first := p.Extents[0]
	return int64(first.TargetData * SectorSize), p.Size(), true
}

// Metadata is a parsed super partition.
type Metadata struct {
	Geometry   Geometry
	Partitions []Partition
}

// Detect reports whether ra holds LP metadata at origin: the geometry magic is
// present where liblp writes it. It reads one block.
func Detect(ra io.ReaderAt, origin int64) bool {
	var magic [4]byte
	if _, err := ra.ReadAt(magic[:], origin+GeometryOffset); err != nil {
		return false
	}
	return binary.LittleEndian.Uint32(magic[:]) == GeometryMagic
}

// Open parses the metadata at origin. The primary slot is used, falling back to
// the other slots (and their backups) when a copy does not validate, so a super
// with one damaged slot still opens.
func Open(ra io.ReaderAt, origin int64) (*Metadata, error) {
	geom, err := readGeometry(ra, origin)
	if err != nil {
		return nil, err
	}
	slotSize := int64(geom.MaxMetadataSize)
	if slotSize <= 0 || slotSize > maxMetadataSize {
		return nil, fmt.Errorf("super metadata size %d out of range", geom.MaxMetadataSize)
	}
	// Layout: reserved block, two geometry copies, primary slots, backup slots.
	primary := origin + GeometryOffset + 2*GeometrySize
	backup := primary + slotSize*int64(geom.SlotCount)

	var firstErr error
	for _, base := range []int64{primary, backup} {
		for slot := uint32(0); slot < geom.SlotCount; slot++ {
			md, err := readSlot(ra, base+int64(slot)*slotSize, geom)
			if err == nil {
				return md, nil
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return nil, fmt.Errorf("no readable super metadata slot: %w", firstErr)
}

func readGeometry(ra io.ReaderAt, origin int64) (Geometry, error) {
	var b [geometryStructSize]byte
	if _, err := ra.ReadAt(b[:], origin+GeometryOffset); err != nil {
		return Geometry{}, fmt.Errorf("read super geometry: %w", err)
	}
	if binary.LittleEndian.Uint32(b[0:4]) != GeometryMagic {
		return Geometry{}, fmt.Errorf("not a super partition")
	}
	if size := binary.LittleEndian.Uint32(b[4:8]); size < geometryStructSize {
		return Geometry{}, fmt.Errorf("super geometry struct size %d too small", size)
	}
	g := Geometry{
		MaxMetadataSize: binary.LittleEndian.Uint32(b[40:44]),
		SlotCount:       binary.LittleEndian.Uint32(b[44:48]),
		LogicalBlockSz:  binary.LittleEndian.Uint32(b[48:52]),
	}
	if g.SlotCount == 0 {
		g.SlotCount = 1
	}
	if g.SlotCount > maxSlots {
		return Geometry{}, fmt.Errorf("super metadata slot count %d out of range", g.SlotCount)
	}
	return g, nil
}

type tableDescriptor struct {
	offset, entries, entrySize uint32
}

func readSlot(ra io.ReaderAt, slot int64, geom Geometry) (*Metadata, error) {
	var hdr [metadataHeaderSize]byte
	if _, err := ra.ReadAt(hdr[:], slot); err != nil {
		return nil, fmt.Errorf("read super metadata header: %w", err)
	}
	if binary.LittleEndian.Uint32(hdr[0:4]) != HeaderMagic {
		return nil, fmt.Errorf("super metadata header magic mismatch")
	}
	headerSize := binary.LittleEndian.Uint32(hdr[8:12])
	if headerSize < metadataHeaderSize || headerSize > uint32(geom.MaxMetadataSize) {
		return nil, fmt.Errorf("super metadata header size %d out of range", headerSize)
	}
	tablesSize := binary.LittleEndian.Uint32(hdr[44:48])
	// The four descriptors follow the header's fixed fields; only the first two
	// (partitions, extents) matter here, but all four are read so the table
	// area's bounds can be checked.
	var descs [4]tableDescriptor
	for i := range descs {
		off := 80 + i*12
		descs[i] = tableDescriptor{
			offset:    binary.LittleEndian.Uint32(hdr[off : off+4]),
			entries:   binary.LittleEndian.Uint32(hdr[off+4 : off+8]),
			entrySize: binary.LittleEndian.Uint32(hdr[off+8 : off+12]),
		}
	}
	// Table offsets are relative to the tables area, which starts right after
	// the header, and the whole area is covered by tablesSize.
	tables := slot + int64(headerSize)
	for i, d := range descs {
		if d.entries == 0 {
			continue
		}
		if d.entrySize == 0 || d.entries > maxEntries {
			return nil, fmt.Errorf("super metadata table %d out of range", i)
		}
		end := uint64(d.offset) + uint64(d.entries)*uint64(d.entrySize)
		if end > uint64(tablesSize) {
			return nil, fmt.Errorf("super metadata table %d runs past the tables area", i)
		}
	}

	extents := make([]Extent, 0, descs[1].entries)
	for i := uint32(0); i < descs[1].entries; i++ {
		var e [24]byte
		off := tables + int64(descs[1].offset) + int64(i)*int64(descs[1].entrySize)
		if _, err := ra.ReadAt(e[:], off); err != nil {
			return nil, fmt.Errorf("read super extent %d: %w", i, err)
		}
		extents = append(extents, Extent{
			NumSectors:   binary.LittleEndian.Uint64(e[0:8]),
			TargetType:   binary.LittleEndian.Uint32(e[8:12]),
			TargetData:   binary.LittleEndian.Uint64(e[12:20]),
			TargetSource: binary.LittleEndian.Uint32(e[20:24]),
		})
	}

	md := &Metadata{Geometry: geom}
	for i := uint32(0); i < descs[0].entries; i++ {
		entrySize := descs[0].entrySize
		if entrySize < 52 {
			return nil, fmt.Errorf("super partition entry size %d too small", entrySize)
		}
		buf := make([]byte, entrySize)
		off := tables + int64(descs[0].offset) + int64(i)*int64(entrySize)
		if _, err := ra.ReadAt(buf, off); err != nil {
			return nil, fmt.Errorf("read super partition %d: %w", i, err)
		}
		p := Partition{
			Name:       cString(buf[:nameLen]),
			Attributes: binary.LittleEndian.Uint32(buf[36:40]),
		}
		first := binary.LittleEndian.Uint32(buf[40:44])
		count := binary.LittleEndian.Uint32(buf[44:48])
		if p.Name == "" {
			continue
		}
		if uint64(first)+uint64(count) > uint64(len(extents)) {
			return nil, fmt.Errorf("partition %s references extents outside the table", p.Name)
		}
		p.Extents = append(p.Extents, extents[first:first+count]...)
		md.Partitions = append(md.Partitions, p)
	}
	if len(md.Partitions) == 0 {
		return nil, fmt.Errorf("super metadata lists no partitions")
	}
	return md, nil
}

// cString trims a NUL-padded fixed-width name.
func cString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// Find returns the named logical partition.
func (m *Metadata) Find(name string) (Partition, bool) {
	for _, p := range m.Partitions {
		if p.Name == name {
			return p, true
		}
	}
	return Partition{}, false
}

// Mapping exposes one logical partition as a byte range: reads are served from
// the extents that make it up, and zero-type extents read as zeroes.
type Mapping struct {
	ra      io.ReaderAt
	origin  int64
	extents []Extent
	starts  []int64 // byte offset within the partition, one per extent
	size    int64
}

// Mapping builds a reader for a partition held in the super at origin.
func (m *Metadata) Mapping(ra io.ReaderAt, origin int64, p Partition) (*Mapping, error) {
	out := &Mapping{ra: ra, origin: origin}
	for _, e := range p.Extents {
		if e.TargetSource != 0 {
			return nil, fmt.Errorf("partition %s has an extent on block device %d, which is not this image", p.Name, e.TargetSource)
		}
		if e.TargetType != TargetLinear && e.TargetType != TargetZero {
			return nil, fmt.Errorf("partition %s has an extent of unknown type %d", p.Name, e.TargetType)
		}
		out.starts = append(out.starts, out.size)
		out.extents = append(out.extents, e)
		out.size += int64(e.NumSectors * SectorSize)
	}
	return out, nil
}

// Size is the partition's size in bytes.
func (mp *Mapping) Size() int64 { return mp.size }

// ReadAt reads from the partition, gathering the extents that cover the range.
func (mp *Mapping) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset")
	}
	if off >= mp.size {
		return 0, io.EOF
	}
	want := len(p)
	done := 0
	for done < want && off+int64(done) < mp.size {
		pos := off + int64(done)
		// The extent holding pos: the last one whose start is <= pos.
		i := sort.Search(len(mp.starts), func(i int) bool { return mp.starts[i] > pos }) - 1
		if i < 0 {
			return done, fmt.Errorf("super partition read outside its extents")
		}
		e := mp.extents[i]
		extentStart := mp.starts[i]
		extentEnd := extentStart + int64(e.NumSectors*SectorSize)
		n := int64(want - done)
		if room := extentEnd - pos; n > room {
			n = room
		}
		if e.TargetType == TargetZero {
			for j := int64(0); j < n; j++ {
				p[done+int(j)] = 0
			}
		} else if _, err := mp.ra.ReadAt(p[done:done+int(n)], mp.origin+int64(e.TargetData*SectorSize)+(pos-extentStart)); err != nil {
			return done, err
		}
		done += int(n)
	}
	if done == 0 {
		return 0, io.EOF
	}
	return done, nil
}
