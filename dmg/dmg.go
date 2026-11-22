package dmg

import (
	"bytes"
	"compress/bzip2"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	lzfse "github.com/go-compressions/lzfse"
	"github.com/ulikunitz/xz"
)

// Sector is the UDIF sector size: chunk tables count 512-byte sectors.
const Sector = 512

// Chunk entry types (blkx table, big-endian).
const (
	chunkZero    = 0x00000000 // fill with zeros
	chunkRaw     = 0x00000001 // uncompressed bytes from the data fork
	chunkIgnore  = 0x00000002 // unallocated; reads as zeros
	chunkADC     = 0x80000004 // Apple Data Compression
	chunkZlib    = 0x80000005 // deflate
	chunkBzip2   = 0x80000006
	chunkLZFSE   = 0x80000007
	chunkLZMA    = 0x80000008
	chunkComment = 0xFFFFFFFF // terminator, covers no sectors
)

// maxCachedChunk bounds the single-slot decompressed-chunk cache, so a huge
// raw or highly compressible chunk never turns into a memory pin.
const maxCachedChunk = 4 << 20

// chunk maps one sector range of the virtual disk to bytes in the file.
type chunk struct {
	start  int64 // absolute byte offset in the virtual disk
	length int64 // uncompressed length (sectors * 512)
	off    int64 // byte offset in the file of the compressed data
	comp   int64 // compressed length
	typ    uint32
}

// Disk is an opened UDIF image: a read-only view of its virtual disk. It
// implements io.ReaderAt and is safe for concurrent use.
type Disk struct {
	ra     io.ReaderAt
	size   int64   // virtual size in bytes
	chunks []chunk // sorted by start, non-overlapping

	// Single-slot cache of the last decompressed chunk. Parallel extractors
	// mostly touch different chunks, so a miss just re-decompresses; the lock
	// only guards the two fields.
	mu        sync.Mutex
	cachedKey int64
	cached    []byte
}

// koly is the fixed 512-byte UDIF trailer at the very end of the file.
type koly struct {
	dataForkOffset int64
	xmlOffset      int64
	xmlLength      int64
	sectorCount    int64
}

// parseKoly decodes the fields the reader needs. All koly integers are
// big-endian; the offsets were verified against images written by hdiutil.
func parseKoly(t []byte) (*koly, error) {
	if len(t) < 512 || string(t[0:4]) != "koly" {
		return nil, errors.New("not a UDIF image (no koly trailer)")
	}
	if v := binary.BigEndian.Uint32(t[4:8]); v != 4 {
		return nil, fmt.Errorf("unsupported UDIF version %d", v)
	}
	k := &koly{
		dataForkOffset: int64(binary.BigEndian.Uint64(t[24:32])),
		xmlOffset:      int64(binary.BigEndian.Uint64(t[216:224])),
		xmlLength:      int64(binary.BigEndian.Uint64(t[224:232])),
		sectorCount:    int64(binary.BigEndian.Uint64(t[492:500])),
	}
	if k.sectorCount <= 0 {
		return nil, errors.New("udif image declares no sectors")
	}
	if k.xmlLength <= 0 || k.xmlLength > 64<<20 {
		return nil, fmt.Errorf("udif plist length %d out of range", k.xmlLength)
	}
	return k, nil
}

// blkx is one entry of the plist's resource-fork/blkx array. Data holds the
// base64-encoded mish block table.
type blkx struct {
	name string
	data []byte
}

// Open reads the trailer, property list and block tables of the UDIF image in
// ra (whose file length is size) and returns its virtual disk.
func Open(ra io.ReaderAt, size int64) (*Disk, error) {
	if size < 512+512 {
		return nil, errors.New("file too small to be a dmg")
	}
	var trailer [512]byte
	if _, err := ra.ReadAt(trailer[:], size-512); err != nil {
		return nil, fmt.Errorf("read udif trailer: %w", err)
	}
	k, err := parseKoly(trailer[:])
	if err != nil {
		return nil, err
	}
	if k.xmlOffset < 0 || k.xmlOffset+k.xmlLength > size {
		return nil, fmt.Errorf("udif plist extent [%d, %d) is outside the file", k.xmlOffset, k.xmlOffset+k.xmlLength)
	}
	xmlBytes := make([]byte, k.xmlLength)
	if _, err := ra.ReadAt(xmlBytes, k.xmlOffset); err != nil {
		return nil, fmt.Errorf("read udif plist: %w", err)
	}
	entries, err := parseBlkxPlist(xmlBytes)
	if err != nil {
		return nil, fmt.Errorf("parse udif plist: %w", err)
	}

	diskSize := k.sectorCount * Sector
	var chunks []chunk
	for _, e := range entries {
		table, err := parseMish(e.data, diskSize)
		if err != nil {
			return nil, fmt.Errorf("blkx %q: %w", e.name, err)
		}
		chunks = append(chunks, table...)
	}

	// Sorted, non-overlapping sector coverage is what the binary-searched
	// reader assumes; a malformed table must fail here rather than silently
	// return wrong bytes.
	sortChunks(chunks)
	for i := 1; i < len(chunks); i++ {
		if chunks[i].start < chunks[i-1].start+chunks[i-1].length {
			return nil, fmt.Errorf("udif chunks overlap at sector %d", chunks[i].start/Sector)
		}
	}

	return &Disk{ra: ra, size: diskSize, chunks: chunks}, nil
}

// sortChunks orders the chunk list by start; insertion sort on a slice that
// is almost always sorted already (tables are written in order) beats a
// general sort and keeps allocation off the path.
func sortChunks(chunks []chunk) {
	for i := 1; i < len(chunks); i++ {
		for j := i; j > 0 && chunks[j].start < chunks[j-1].start; j-- {
			chunks[j], chunks[j-1] = chunks[j-1], chunks[j]
		}
	}
}

// Size returns the virtual disk size in bytes.
func (d *Disk) Size() int64 { return d.size }

// Close releases nothing; the caller owns the underlying reader.
func (d *Disk) Close() error { return nil }

// ReadAt reads len(p) bytes at off from the virtual disk, following the
// io.ReaderAt contract (a read that reaches the end returns io.EOF after the
// bytes it could provide).
func (d *Disk) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= d.size {
		return 0, io.EOF
	}
	wanted := len(p)
	if n := d.size - off; int64(wanted) > n {
		p = p[:n]
	}

	total := 0
	for len(p) > 0 {
		i := d.chunkAt(off)
		if i < 0 {
			// Before the first chunk: zeros up to the next one.
			n := d.zeroRun(p, off, d.chunks[0].start)
			total += n
			p, off = p[n:], off+int64(n)
			continue
		}
		c := d.chunks[i]
		if off >= c.start+c.length {
			// In a gap between chunks: zeros up to the next one.
			var next int64 = 1 << 62
			if i+1 < len(d.chunks) {
				next = d.chunks[i+1].start
			}
			n := d.zeroRun(p, off, next)
			total += n
			p, off = p[n:], off+int64(n)
			continue
		}
		data, err := d.chunkData(c)
		if err != nil {
			return total, err
		}
		skip := off - c.start
		n := int64(len(p))
		if c.length-skip < n {
			n = c.length - skip
		}
		if data == nil {
			clear(p[:n]) // zero/ignore chunk
		} else {
			copy(p[:n], data[skip:skip+n])
		}
		total += int(n)
		p, off = p[n:], off+n
	}
	if total == 0 {
		return 0, io.EOF
	}
	if total < wanted {
		return total, io.EOF // the disk ended inside the request
	}
	return total, nil
}

// zeroRun fills p with zeros, stopping at limit (the next chunk start or an
// artificial bound), and returns how many bytes it filled.
func (d *Disk) zeroRun(p []byte, off, limit int64) int {
	n := int64(len(p))
	if limit-off < n {
		n = limit - off
	}
	if n <= 0 {
		n = 1 // never stall: the caller's loop always advances
	}
	clear(p[:n])
	return int(n)
}

// chunkAt returns the index of the last chunk starting at or before off, or -1.
func (d *Disk) chunkAt(off int64) int {
	lo, hi := 0, len(d.chunks)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		if d.chunks[mid].start <= off {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return hi
}

// chunkData returns the chunk's decompressed contents, through the one-slot
// cache when it holds this chunk.
func (d *Disk) chunkData(c chunk) ([]byte, error) {
	d.mu.Lock()
	if d.cachedKey == c.start && d.cached != nil {
		data := d.cached
		d.mu.Unlock()
		return data, nil
	}
	d.mu.Unlock()

	switch c.typ {
	case chunkZero, chunkIgnore:
		return nil, nil
	case chunkRaw:
		data := make([]byte, c.length)
		if _, err := d.ra.ReadAt(data, c.off); err != nil {
			return nil, fmt.Errorf("read raw chunk at %d: %w", c.off, err)
		}
		d.store(c, data)
		return data, nil
	}

	comp := make([]byte, c.comp)
	if _, err := d.ra.ReadAt(comp, c.off); err != nil {
		return nil, fmt.Errorf("read chunk at %d: %w", c.off, err)
	}
	data, err := decompress(c.typ, comp, c.length)
	if err != nil {
		return nil, fmt.Errorf("chunk at sector %d: %w", c.start/Sector, err)
	}
	d.store(c, data)
	return data, nil
}

func (d *Disk) store(c chunk, data []byte) {
	if len(data) > maxCachedChunk {
		return
	}
	d.mu.Lock()
	d.cachedKey, d.cached = c.start, data
	d.mu.Unlock()
}

// decompress decodes one full chunk of the given type to exactly want bytes.
func decompress(typ uint32, comp []byte, want int64) ([]byte, error) {
	switch typ {
	case chunkZlib:
		zr, err := zlib.NewReader(bytes.NewReader(comp))
		if err != nil {
			return nil, err
		}
		return readExactly(zr, want, "zlib")
	case chunkBzip2:
		return readExactly(bzip2.NewReader(bytes.NewReader(comp)), want, "bzip2")
	case chunkLZMA:
		// hdiutil writes whole xz container streams into the chunks it labels
		// "lzma" (UDMO/ULMO), so the xz reader is what decodes them.
		zr, err := xz.NewReader(bytes.NewReader(comp))
		if err != nil {
			return nil, err
		}
		return readExactly(zr, want, "xz")
	case chunkLZFSE:
		out, err := lzfse.DecompressLimit(comp, int(want))
		if err != nil {
			return nil, err
		}
		if int64(len(out)) != want {
			return nil, fmt.Errorf("lzfse chunk decoded %d bytes, want %d", len(out), want)
		}
		return out, nil
	case chunkADC:
		return adcDecompress(comp, want)
	default:
		return nil, fmt.Errorf("unknown udif chunk type %#x", typ)
	}
}

func readExactly(r io.Reader, want int64, what string) ([]byte, error) {
	out := make([]byte, want)
	n, err := io.ReadFull(r, out)
	if err != nil {
		return nil, fmt.Errorf("%s chunk decoded %d of %d bytes: %w", what, n, want, err)
	}
	return out, nil
}

// parseMish decodes one blkx resource: a mish block table. Chunk sector
// numbers are relative to the table's first sector (verified against
// hdiutil-written images: the chunk lengths of a partition's table sum to
// exactly that partition's sector count).
func parseMish(data []byte, diskSize int64) ([]chunk, error) {
	if len(data) < 204 || string(data[0:4]) != "mish" {
		return nil, errors.New("no mish block table")
	}
	firstSector := int64(binary.BigEndian.Uint64(data[8:16]))
	sectorCount := int64(binary.BigEndian.Uint64(data[16:24]))
	dataOffset := int64(binary.BigEndian.Uint64(data[24:32]))
	numChunks := binary.BigEndian.Uint32(data[200:204])
	if firstSector < 0 || sectorCount < 0 || firstSector+sectorCount > diskSize/Sector {
		return nil, fmt.Errorf("sector range [%d, %d) does not fit the disk", firstSector, firstSector+sectorCount)
	}
	if dataOffset < 0 {
		return nil, fmt.Errorf("negative data offset %d", dataOffset)
	}
	if numChunks == 0 || int64(numChunks)*40 > int64(len(data)-204) {
		return nil, fmt.Errorf("chunk count %d does not fit the table", numChunks)
	}

	var out []chunk
	pos := int64(0) // chunk sectors are relative to the table's first sector
	for i := uint32(0); i < numChunks; i++ {
		e := data[204+i*40:]
		typ := binary.BigEndian.Uint32(e[0:4])
		start := int64(binary.BigEndian.Uint64(e[8:16]))
		count := int64(binary.BigEndian.Uint64(e[16:24]))
		off := int64(binary.BigEndian.Uint64(e[24:32]))
		length := int64(binary.BigEndian.Uint64(e[32:40]))
		if typ == chunkComment {
			continue
		}
		if start < 0 || count < 0 || off < 0 || length < 0 {
			return nil, fmt.Errorf("malformed chunk %d (type %#x, sectors %d..%d)", i, typ, start, start+count)
		}
		if count == 0 {
			// Zero-length placeholder chunks appear in UDRO tables; they cover
			// no sectors, so they neither advance the position nor matter.
			continue
		}
		if start != pos {
			return nil, fmt.Errorf("chunk %d starts at sector %d, want %d", i, start, pos)
		}
		pos += count
		c := chunk{
			start:  (firstSector + start) * Sector,
			length: count * Sector,
			off:    dataOffset + off,
			comp:   length,
			typ:    typ,
		}
		if c.start+c.length > diskSize {
			return nil, fmt.Errorf("chunk covers sectors beyond the disk (%d..%d)", c.start/Sector, (c.start+c.length)/Sector)
		}
		switch typ {
		case chunkZero, chunkIgnore:
			c.off, c.comp = 0, 0
		case chunkRaw:
			if c.comp == 0 {
				c.comp = c.length
			}
		}
		out = append(out, c)
	}
	if pos != sectorCount {
		return nil, fmt.Errorf("chunks cover %d sectors, table declares %d", pos, sectorCount)
	}
	return out, nil
}

// plistNode is one node of the image plist: a dict or array container, or a
// scalar (string/integer/data) with its raw text.
type plistNode struct {
	kind     string // "dict", "array", "string", "integer", "data"
	keys     []string
	children []*plistNode
	text     []byte
}

// parseBlkxPlist walks the image's XML property list and returns the blkx
// entries of its resource fork. Only the plist subset an image actually uses
// is handled: nested dict/array/string/integer/data nodes.
func parseBlkxPlist(data []byte) ([]blkx, error) {
	var root *plistNode
	// stack of containers still being filled; the last key seen in each dict
	var stack []*plistNode
	var pendingKey string

	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "dict", "array":
				n := &plistNode{kind: t.Name.Local}
				if len(stack) == 0 {
					root = n
				} else {
					parent := stack[len(stack)-1]
					if parent.kind == "dict" && pendingKey != "" {
						parent.keys = append(parent.keys, pendingKey)
						parent.children = append(parent.children, n)
						pendingKey = ""
					} else if parent.kind == "array" {
						parent.children = append(parent.children, n)
					}
				}
				stack = append(stack, n)
			case "key":
				var raw []byte
				if err := dec.DecodeElement(&raw, &t); err != nil {
					return nil, err
				}
				pendingKey = string(raw)
			case "string", "integer", "data":
				var raw []byte
				if err := dec.DecodeElement(&raw, &t); err != nil {
					return nil, err
				}
				n := &plistNode{kind: t.Name.Local, text: raw}
				if len(stack) == 0 {
					root = n
				} else {
					parent := stack[len(stack)-1]
					if parent.kind == "dict" && pendingKey != "" {
						parent.keys = append(parent.keys, pendingKey)
						parent.children = append(parent.children, n)
						pendingKey = ""
					} else if parent.kind == "array" {
						parent.children = append(parent.children, n)
					}
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "dict", "array":
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
		}
	}
	if root == nil || root.kind != "dict" {
		return nil, errors.New("plist has no root dict")
	}
	fork := root.child("resource-fork")
	if fork == nil {
		return nil, errors.New("plist has no resource-fork")
	}
	arr := fork.child("blkx")
	if arr == nil {
		return nil, errors.New("resource-fork has no blkx array")
	}

	var out []blkx
	for _, item := range arr.children {
		if item.kind != "dict" {
			continue
		}
		e := blkx{name: item.stringValue("CFName")}
		raw := item.child("Data")
		if raw == nil || raw.kind != "data" {
			continue
		}
		text := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, string(raw.text))
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, fmt.Errorf("blkx %q: %w", e.name, err)
		}
		e.data = decoded
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, errors.New("plist declares no blkx entries")
	}
	return out, nil
}

// small navigation helpers for the parsed plist nodes

func (n *plistNode) child(key string) *plistNode {
	for i, k := range n.keys {
		if k == key {
			return n.children[i]
		}
	}
	return nil
}

func (n *plistNode) stringValue(key string) string {
	c := n.child(key)
	if c == nil {
		return ""
	}
	return string(c.text)
}
