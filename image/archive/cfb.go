package archive

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// COM Structured Storage (CFB, "OLE compound file") reading: the container
// behind .msi installers, .msp patches and legacy Office documents. Spec:
// [MS-CFB] https://learn.microsoft.com/openspecs/windows_protocols/ms-cfb/
//
// The file is a flat pool of sectors. FAT sectors hold a chain map (next
// sector per sector), DIFAT sectors list the FAT sectors themselves, and a
// directory of 128-byte entries forms one tree per storage. Small streams
// (below the mini-stream cutoff) live packed into 64-byte mini sectors inside
// the root entry's stream, chained by the mini FAT.
//
// Field offsets were verified against an MSI written by wixl/msitools and the
// real files it ships.

const (
	cfbMagic = "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"

	cfbMaxRegSectors   = 1 << 24 // cap a FAT chain walk at 16M sectors
	cfbMaxDirEntries   = 1 << 18
	cfbMaxDifatSectors = 1 << 12
)

// special FAT chain values ([MS-CFB] 2.2 Fat Entries).
const (
	cfbFree       = 0xFFFFFFFF
	cfbEndOfChain = 0xFFFFFFFE
	cfbFatSect    = 0xFFFFFFFC
	cfbDifSect    = 0xFFFFFFFD
)

// cfbFileTime converts a Windows FILETIME (100ns units since 1601-01-01) to
// time.Time; a zero field stays the zero time.
func cfbFileTime(b []byte) time.Time {
	v := binary.LittleEndian.Uint64(b)
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(v)*100-116444736000000000).UTC()
}

// decodeStreamName undoes MSI's stream-name compression (libmsi's
// decode_streamname): table stream names carry a U+4840 prefix, and pairs of
// MIME-able characters (alnum, '.', '_') are packed into single code points in
// 0x3800..0x47FF — one character in the low 7 bits, the other coded by the
// high byte XOR 0xA0. Plain UTF-16 names pass through unchanged, so legacy
// Office documents and summary streams decode as themselves. The algorithm
// runs on the UTF-8 byte stream the code units produce, exactly as libmsi
// sees it.
func decodeStreamName(units []uint16) string {
	var b bytes.Buffer
	for _, u := range units {
		switch {
		case u < 0x80:
			b.WriteByte(byte(u))
		case u < 0x800:
			b.WriteByte(0xC0 | byte(u>>6))
			b.WriteByte(0x80 | byte(u&0x3F))
		default:
			b.WriteByte(0xE0 | byte(u>>12))
			b.WriteByte(0x80 | byte((u>>6)&0x3F))
			b.WriteByte(0x80 | byte(u&0x3F))
		}
	}

	mime2utf := func(x int) byte {
		switch {
		case x < 10:
			return byte('0' + x)
		case x < 36:
			return byte('A' + x - 10)
		case x < 62:
			return byte('a' + x - 36)
		case x == 62:
			return '.'
		default:
			return '_'
		}
	}

	buf := b.Bytes()
	var out []byte
	p := 0
	for p < len(buf) && buf[p] != 0 {
		ch := buf[p]
		if (ch == 0xE3 && p+1 < len(buf) && buf[p+1] >= 0xA0) ||
			(ch == 0xE4 && p+1 < len(buf) && buf[p+1] < 0xA0) {
			// Two packed characters.
			out = append(out, mime2utf(int(buf[p+2]&0x7F)), mime2utf(int(buf[p+1]^0xA0)))
			p += 3
			continue
		}
		if ch == 0xE4 && p+1 < len(buf) && buf[p+1] == 0xA0 {
			// One character.
			out = append(out, mime2utf(int(buf[p+2]&0x7F)))
			p += 3
			continue
		}
		out = append(out, buf[p])
		p++
		if ch >= 0xC1 && p < len(buf) {
			out = append(out, buf[p])
			p++
		}
		if ch >= 0xE0 && p < len(buf) {
			out = append(out, buf[p])
			p++
		}
		if ch >= 0xF0 && p < len(buf) {
			out = append(out, buf[p])
			p++
		}
	}
	// The U+4840 prefix marks MSI table streams; the listing shows the bare
	// name (msiinfo does the same).
	s := string(out)
	return strings.TrimPrefix(s, "\u4840")
}

type cfbEntry struct {
	name  string
	typ   byte // 1 storage, 2 stream, 5 root
	left  uint32
	right uint32
	child uint32
	start uint32
	size  int64
	mtime time.Time
}

type cfb struct {
	f          *os.File
	sectorSize int64
	miniSize   int64
	miniCutoff int64
	fat        []uint32
	miniFat    []uint32
	dir        []cfbEntry
}

// openCFB parses the header, FAT/DIFAT and directory of the compound file.
func openCFB(f *os.File) (*cfb, error) {
	var hdr [512]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return nil, fmt.Errorf("read cfb header: %w", err)
	}
	if string(hdr[0:8]) != cfbMagic {
		return nil, fmt.Errorf("not a compound file")
	}
	if v := binary.LittleEndian.Uint16(hdr[28:30]); v != 0xFFFE {
		return nil, fmt.Errorf("cfb byte order %#x is not little-endian", v)
	}
	sectorShift := int(binary.LittleEndian.Uint16(hdr[30:32]))
	miniShift := int(binary.LittleEndian.Uint16(hdr[32:34]))
	if sectorShift != 9 && sectorShift != 12 {
		return nil, fmt.Errorf("cfb sector shift %d is not 9 or 12", sectorShift)
	}
	if miniShift != 6 {
		return nil, fmt.Errorf("cfb mini sector shift %d is not 6", miniShift)
	}

	numFATSectors := binary.LittleEndian.Uint32(hdr[44:48])
	firstDirSector := binary.LittleEndian.Uint32(hdr[48:52])
	miniCutoff := int64(binary.LittleEndian.Uint32(hdr[56:60]))
	firstMiniFAT := binary.LittleEndian.Uint32(hdr[60:64])
	numMiniFAT := binary.LittleEndian.Uint32(hdr[64:68])
	firstDIFAT := binary.LittleEndian.Uint32(hdr[68:72])
	numDIFAT := binary.LittleEndian.Uint32(hdr[72:76])

	sectorSize := int64(1) << sectorShift
	c := &cfb{
		f:          f,
		sectorSize: sectorSize,
		miniSize:   int64(1) << miniShift,
		miniCutoff: miniCutoff,
	}

	// DIFAT: the first 109 entries live in the header; the rest chain through
	// DIFAT sectors (each holding sectorSize/4-1 entries, the last pointing to
	// the next DIFAT sector).
	fatSectors := make([]uint32, 0, numFATSectors)
	for i := 0; i < 109; i++ {
		s := binary.LittleEndian.Uint32(hdr[76+i*4:])
		if s == cfbFree || s == cfbEndOfChain || s == cfbFatSect || s == cfbDifSect {
			continue
		}
		fatSectors = append(fatSectors, s)
	}
	// Some writers (wixl) store "none" as the ENDOFCHAIN sentinel in the DIFAT
	// count; normalize the sentinels before looping.
	difatCount := numDIFAT
	if difatCount == cfbEndOfChain || difatCount == cfbFree {
		difatCount = 0
	}
	difat := firstDIFAT
	for i := uint32(0); i < difatCount && i < cfbMaxDifatSectors; i++ {
		buf, err := c.readSectorRaw(difat)
		if err != nil {
			return nil, fmt.Errorf("read difat sector %d: %w", difat, err)
		}
		n := int(sectorSize/4) - 1
		for j := 0; j < n; j++ {
			s := binary.LittleEndian.Uint32(buf[j*4:])
			if s != cfbFree && s != cfbEndOfChain {
				fatSectors = append(fatSectors, s)
			}
		}
		next := binary.LittleEndian.Uint32(buf[(sectorSize/4-1)*4:])
		if next == cfbEndOfChain || next == cfbFree {
			break
		}
		difat = next
	}

	// FAT sectors, concatenated: one u32 next-pointer per regular sector. The
	// DIFAT lists exactly numFATSectors of them; trailing entries are filler.
	if numFATSectors > 0 {
		if uint32(len(fatSectors)) < numFATSectors {
			return nil, fmt.Errorf("cfb declares %d FAT sectors but lists %d", numFATSectors, len(fatSectors))
		}
		if uint32(len(fatSectors)) > numFATSectors {
			fatSectors = fatSectors[:numFATSectors]
		}
	}
	for _, s := range fatSectors {
		buf, err := c.readSectorRaw(s)
		if err != nil {
			return nil, fmt.Errorf("read fat sector %d: %w", s, err)
		}
		for j := 0; j+4 <= int(sectorSize); j += 4 {
			c.fat = append(c.fat, binary.LittleEndian.Uint32(buf[j:]))
		}
	}

	// Directory: a chain of sectors holding 128-byte entries.
	dirBuf, err := c.readChain(firstDirSector, 0) // size 0 = whole chain
	if err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}
	n := len(dirBuf) / 128
	if n > cfbMaxDirEntries {
		n = cfbMaxDirEntries
	}
	for i := 0; i < n; i++ {
		e := dirBuf[i*128:]
		typ := e[66]
		if typ == 0 { // unallocated
			continue
		}
		nameLen := int(binary.LittleEndian.Uint16(e[64:66]))
		if nameLen < 2 || nameLen > 64 || nameLen%2 != 0 {
			return nil, fmt.Errorf("cfb directory entry %d has bad name length %d", i, nameLen)
		}
		units := make([]uint16, nameLen/2)
		for j := range units {
			units[j] = binary.LittleEndian.Uint16(e[j*2:])
		}
		name := decodeStreamName(units)
		size := int64(binary.LittleEndian.Uint64(e[120:128]))
		if sectorSize == 512 {
			// v3 files only have 32-bit stream sizes; the high half is
			// unspecified and wixl fills it with garbage.
			size = int64(binary.LittleEndian.Uint32(e[120:124]))
		}
		c.dir = append(c.dir, cfbEntry{
			name:  name,
			typ:   typ,
			left:  binary.LittleEndian.Uint32(e[68:72]),
			right: binary.LittleEndian.Uint32(e[72:76]),
			child: binary.LittleEndian.Uint32(e[76:80]),
			start: binary.LittleEndian.Uint32(e[116:120]),
			size:  size,
			mtime: cfbFileTime(e[108:116]),
		})
	}

	// Mini FAT (best effort: some writers emit junk here when unused).
	if numMiniFAT > 0 && firstMiniFAT < uint32(len(c.fat)) && firstMiniFAT != cfbFree && firstMiniFAT != cfbEndOfChain {
		buf, err := c.readChain(firstMiniFAT, 0)
		if err == nil {
			for j := 0; j+4 <= len(buf); j += 4 {
				c.miniFat = append(c.miniFat, binary.LittleEndian.Uint32(buf[j:]))
			}
		}
	}
	return c, nil
}

// rootID returns the index of the root entry, or -1.
func (c *cfb) rootID() int {
	for i := range c.dir {
		if c.dir[i].typ == 5 {
			return i
		}
	}
	return -1
}

// readSectorRaw reads one full sector. MS-CFB numbers sectors from 0 AFTER
// the 512-byte header, so sector N lives at (N+1)*sectorSize.
func (c *cfb) readSectorRaw(s uint32) ([]byte, error) {
	if int64(s)*c.sectorSize >= 1<<62 {
		return nil, fmt.Errorf("sector %d out of range", s)
	}
	buf := make([]byte, c.sectorSize)
	if _, err := c.f.ReadAt(buf, (int64(s)+1)*c.sectorSize); err != nil {
		return nil, err
	}
	return buf, nil
}

// chainSectors walks the FAT chain from start, with a cycle guard.
func (c *cfb) chainSectors(start uint32) ([]uint32, error) {
	if start == cfbFree || start == cfbEndOfChain {
		return nil, nil
	}
	out := make([]uint32, 0, 16)
	seen := map[uint32]bool{}
	cur := start
	for {
		if seen[cur] {
			return nil, fmt.Errorf("cfb chain from sector %d loops", start)
		}
		seen[cur] = true
		out = append(out, cur)
		if int64(len(out)) > cfbMaxRegSectors {
			return nil, fmt.Errorf("cfb chain from sector %d is too long", start)
		}
		if cur >= uint32(len(c.fat)) {
			return nil, fmt.Errorf("cfb chain references sector %d beyond the FAT", cur)
		}
		next := c.fat[cur]
		if next == cfbEndOfChain || next == cfbFree || next == cfbFatSect || next == cfbDifSect {
			break
		}
		cur = next
	}
	return out, nil
}

// readChain concatenates a FAT chain. A size of 0 means the whole chain.
func (c *cfb) readChain(start uint32, size int64) ([]byte, error) {
	sectors, err := c.chainSectors(start)
	if err != nil {
		return nil, err
	}
	if len(sectors) == 0 {
		return nil, nil
	}
	buf := make([]byte, int64(len(sectors))*c.sectorSize)
	for i, s := range sectors {
		if _, err := c.f.ReadAt(buf[int64(i)*c.sectorSize:], (int64(s)+1)*c.sectorSize); err != nil {
			return nil, fmt.Errorf("read sector %d: %w", s, err)
		}
	}
	if size > 0 && size < int64(len(buf)) {
		buf = buf[:size]
	}
	return buf, nil
}

// miniStream returns the root entry's stream: the pool of mini sectors.
func (c *cfb) miniStream() ([]byte, error) {
	root := c.rootID()
	if root < 0 {
		return nil, fmt.Errorf("cfb has no root entry")
	}
	return c.readChain(c.dir[root].start, c.dir[root].size)
}

// readStream returns one stream's bytes, through the mini stream when small.
func (c *cfb) readStream(e *cfbEntry) ([]byte, error) {
	if c.miniCutoff > 0 && e.size < c.miniCutoff && len(c.miniFat) > 0 {
		pool, err := c.miniStream()
		if err != nil {
			return nil, err
		}
		return c.readMiniChain(pool, e.start, e.size)
	}
	return c.readChain(e.start, e.size)
}

// readMiniChain assembles a mini stream slice from 64-byte mini sectors.
func (c *cfb) readMiniChain(pool []byte, start uint32, size int64) ([]byte, error) {
	out := make([]byte, 0, size)
	seen := map[uint32]bool{}
	cur := start
	for int64(len(out)) < size {
		if seen[cur] {
			return nil, fmt.Errorf("mini chain from %d loops", start)
		}
		seen[cur] = true
		begin := int64(cur) * c.miniSize
		if begin+c.miniSize > int64(len(pool)) {
			return nil, fmt.Errorf("mini sector %d outside the mini stream", cur)
		}
		out = append(out, pool[begin:begin+c.miniSize]...)
		if cur >= uint32(len(c.miniFat)) {
			return nil, fmt.Errorf("mini chain references %d beyond the mini FAT", cur)
		}
		next := c.miniFat[cur]
		if next == cfbEndOfChain || next == cfbFree {
			break
		}
		cur = next
	}
	if int64(len(out)) > size {
		out = out[:size]
	}
	return out, nil
}

// walk visits every entry of the directory tree in storage order, passing
// paths (slash-separated, root storage itself excluded). Cycles are guarded.
func (c *cfb) walk(visit func(path string, e *cfbEntry) error) error {
	root := c.rootID()
	if root < 0 {
		return fmt.Errorf("cfb has no root entry")
	}
	visited := map[uint32]bool{}
	var descend func(id uint32, dir string) error
	descend = func(id uint32, dir string) error {
		if id == cfbFree || id == cfbEndOfChain || id >= uint32(len(c.dir)) {
			return nil
		}
		if visited[id] {
			return fmt.Errorf("cfb directory entry %d is visited twice", id)
		}
		visited[id] = true
		e := &c.dir[id]
		if err := descend(e.left, dir); err != nil {
			return err
		}
		if e.typ == 1 || e.typ == 2 {
			p := e.name
			if dir != "" {
				p = dir + "/" + e.name
			}
			if err := visit(p, e); err != nil {
				return err
			}
			if e.typ == 1 {
				if err := descend(e.child, p); err != nil {
					return err
				}
			}
		}
		if err := descend(e.right, dir); err != nil {
			return err
		}
		return nil
	}
	return descend(c.dir[root].child, "")
}

// streamReaderAt exposes one stream as random access over its sector chain,
// reading sectors from the file only as they are requested. Regular streams
// map offsets through the FAT chain; mini streams through the mini FAT into
// the root entry's pool — neither is materialized.
func (c *cfb) streamReaderAt(e *cfbEntry) (io.ReaderAt, error) {
	if c.miniCutoff > 0 && e.size < c.miniCutoff && len(c.miniFat) > 0 {
		// Mini stream: sector k of this stream is mini sector e.start+k of the
		// mini chain, which lives at regular sector chain[k] of the root's
		// pool. Walk both chains up front (in-memory linked lists, no I/O).
		root := c.rootID()
		if root < 0 {
			return nil, fmt.Errorf("cfb has no root entry")
		}
		pool, err := c.chainSectors(c.dir[root].start)
		if err != nil {
			return nil, err
		}
		// Mini sectors of this stream live packed inside the root pool's
		// regular sectors: mini sector k is pool regular sector k/perSector at
		// byte offset (k%perSector)*64.
		perSector := int(c.sectorSize / c.miniSize)
		var offs []int64
		cur := e.start
		seen := map[uint32]bool{}
		for int64(len(offs))*c.miniSize < e.size {
			if seen[cur] {
				return nil, fmt.Errorf("mini chain loops")
			}
			seen[cur] = true
			if cur >= uint32(len(c.miniFat)) {
				return nil, fmt.Errorf("mini chain references %d beyond the mini FAT", cur)
			}
			if int(cur)/perSector >= len(pool) {
				return nil, fmt.Errorf("mini sector %d outside the pool", cur)
			}
			offs = append(offs, (int64(pool[cur/uint32(perSector)])+1)*c.sectorSize+int64(cur%uint32(perSector))*c.miniSize)
			next := c.miniFat[cur]
			if next == cfbEndOfChain || next == cfbFree {
				break
			}
			cur = next
		}
		return &chainReader{c: c, offs: offs, sectorSize: c.miniSize, size: e.size}, nil
	}
	sectors, err := c.chainSectors(e.start)
	if err != nil {
		return nil, err
	}
	offs := make([]int64, len(sectors))
	for i, s := range sectors {
		offs[i] = (int64(s) + 1) * c.sectorSize
	}
	return &chainReader{c: c, offs: offs, sectorSize: c.sectorSize, size: e.size}, nil
}

// chainReader serves random access over a pre-walked sector chain, holding
// each sector's absolute byte offset so reads touch the file directly.
type chainReader struct {
	c          *cfb
	offs       []int64
	sectorSize int64
	size       int64
}

func (r *chainReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= r.size {
		if off == r.size && len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	if int64(len(p)) > r.size-off {
		p = p[:r.size-off]
	}
	total := 0
	for len(p) > 0 {
		k := int(off / r.sectorSize)
		if k >= len(r.offs) {
			return total, io.EOF
		}
		within := off % r.sectorSize
		n := r.sectorSize - within
		if int64(len(p)) < n {
			n = int64(len(p))
		}
		if _, err := r.c.f.ReadAt(p[:n], r.offs[k]+within); err != nil {
			return total, err
		}
		p, off = p[n:], off+n
		total += int(n)
	}
	return total, nil
}
