package archive

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz/lzma"

	"github.com/ejfkdev/udf/fsview"
)

// A Unity asset bundle ("UnityFS", Unity 5.3+) packs the serialized assets of a
// game — a CAB-* payload, plus .resS/.resource streams — into compressed
// blocks. udf lists the bundle's files and extracts them, decompressing only
// the blocks a requested file overlaps.
//
// Unity never documented the container, so this follows what the community
// implementations do (UnityPy): a variable-length header, an optional 16-byte
// alignment, a block table and a node table (optionally at the end of the file)
// and then the data blocks, each compressed on its own.
//
// The container itself is big-endian, unlike the serialized assets inside it
// (which this reader never decodes). Older containers
// (UnityWeb/UnityRaw/UnityArchive) and encrypted bundles (Unity CN) are
// recognised but not decoded: the former are rare, the latter need a key that
// ships with the build.
const (
	unityFSMagic = "UnityFS\x00"

	unityFlagCompressionMask = 0x3F
	unityFlagInfoAtTheEnd    = 0x80
	unityFlagPadAtStart      = 0x200 // newer flag sets; encryption in older ones
	unityFlagEncrypted       = 0x1400

	unityCompressionNone  = 0
	unityCompressionLZMA  = 1
	unityCompressionLZ4   = 2
	unityCompressionLZ4HC = 3
	unityCompressionLZHAM = 4

	// unityMaxInfoSize bounds the block/node tables, so a corrupt header
	// cannot make the reader allocate without limit.
	unityMaxInfoSize = 64 << 20
	unityMaxNodes    = 1 << 20
)

type unityBlock struct {
	uncompressed uint32
	compressed   uint32
	flags        uint16
}

type unityNode struct {
	offset int64
	size   int64
	flags  uint32
	path   string
}

type unityArchive struct {
	path string

	version uint32
	player  string
	engine  string

	blocks    []unityBlock
	nodes     []unityNode
	blockOffs []int64 // file offset of each block's data
	total     int64   // size of the decompressed block stream
}

// openUnityFS parses a UnityFS bundle's header and tables.
func openUnityFS(path string) (*unityArchive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	c := &fileCursor{f: f, size: st.Size()}
	magic, err := c.bytes(len(unityFSMagic))
	if err != nil {
		return nil, err
	}
	if string(magic) != unityFSMagic {
		return nil, fmt.Errorf("not a UnityFS asset bundle")
	}
	a := &unityArchive{path: path}
	if a.version, err = c.u32(); err != nil {
		return nil, err
	}
	if a.player, err = c.cstring(unityMaxStringLen); err != nil {
		return nil, err
	}
	if a.engine, err = c.cstring(unityMaxStringLen); err != nil {
		return nil, err
	}
	fileSize, err := c.u64()
	if err != nil {
		return nil, err
	}
	if fileSize > st.Size() {
		return nil, fmt.Errorf("bundle claims %d bytes but the file holds %d", fileSize, st.Size())
	}
	compressedSize, err := c.u32()
	if err != nil {
		return nil, err
	}
	uncompressedSize, err := c.u32()
	if err != nil {
		return nil, err
	}
	flags, err := c.u32()
	if err != nil {
		return nil, err
	}
	if compressedSize > unityMaxInfoSize || uncompressedSize > unityMaxInfoSize {
		return nil, fmt.Errorf("implausible bundle information size")
	}

	// Newer bundles (and 2019.4.15+) align the header to 16 bytes before the
	// block information.
	if a.version >= 7 || unityVersionAtLeast(a.engine, 2019, 4, 15) {
		c.align(16)
	}

	compression := flags & unityFlagCompressionMask
	oldFlagSet := unityUsesOldFlagSet(a.engine)
	switch {
	case oldFlagSet && flags&unityFlagPadAtStart != 0, flags&unityFlagEncrypted != 0:
		return nil, fmt.Errorf("asset bundle is encrypted (Unity CN); a key is required")
	case compression == unityCompressionLZHAM:
		return nil, fmt.Errorf("asset bundle uses LZHAM compression, which is not supported")
	}

	// The tables sit either right here or at the end of the file.
	infoStart := c.pos
	if flags&unityFlagInfoAtTheEnd != 0 {
		infoStart = st.Size() - int64(compressedSize)
		c.pos = infoStart
	}
	infoBytes, err := c.bytes(int(compressedSize))
	if err != nil {
		return nil, err
	}
	info, err := unityDecompress(infoBytes, uncompressedSize, compression)
	if err != nil {
		return nil, fmt.Errorf("bundle information: %w", err)
	}
	if err := a.parseInfo(info); err != nil {
		return nil, err
	}

	// Where the data blocks begin: after the header when the tables were at
	// the end, after the tables otherwise.
	if flags&unityFlagInfoAtTheEnd != 0 {
		c.pos = infoStart
	} else {
		c.pos = infoStart + int64(compressedSize)
		if !oldFlagSet && flags&unityFlagPadAtStart != 0 {
			c.align(16)
		}
	}
	for i := range a.blocks {
		a.blockOffs = append(a.blockOffs, c.pos)
		if err := c.skip(int64(a.blocks[i].compressed)); err != nil {
			return nil, fmt.Errorf("block %d extends past the end of the file", i)
		}
	}
	return a, nil
}

// parseInfo reads the block table and the node table from the decompressed
// information blob.
func (a *unityArchive) parseInfo(info []byte) error {
	c := &memCursor{b: info}
	if _, err := c.bytes(16); err != nil { // uncompressed data hash
		return fmt.Errorf("bundle information is truncated")
	}
	blockCount, err := c.u32()
	if err != nil {
		return err
	}
	if blockCount > unityMaxNodes {
		return fmt.Errorf("implausible block count %d", blockCount)
	}
	for i := uint32(0); i < blockCount; i++ {
		uncompressed, err := c.u32()
		if err != nil {
			return err
		}
		compressed, err := c.u32()
		if err != nil {
			return err
		}
		flags, err := c.u16()
		if err != nil {
			return err
		}
		a.blocks = append(a.blocks, unityBlock{
			uncompressed: uncompressed,
			compressed:   compressed,
			flags:        flags,
		})
	}
	nodeCount, err := c.u32()
	if err != nil {
		return err
	}
	if nodeCount > unityMaxNodes {
		return fmt.Errorf("implausible node count %d", nodeCount)
	}
	for i := uint32(0); i < nodeCount; i++ {
		offset, err := c.i64()
		if err != nil {
			return err
		}
		size, err := c.i64()
		if err != nil {
			return err
		}
		flags, err := c.u32()
		if err != nil {
			return err
		}
		path, err := c.cstring(unityMaxStringLen)
		if err != nil {
			return err
		}
		a.nodes = append(a.nodes, unityNode{offset: offset, size: size, flags: flags, path: path})
	}
	for i := range a.blocks {
		a.total += int64(a.blocks[i].uncompressed)
	}
	for i := range a.nodes {
		n := &a.nodes[i]
		if n.offset < 0 || n.size < 0 || n.offset+n.size > a.total {
			return fmt.Errorf("node %q lies outside the bundle data", n.path)
		}
	}
	return nil
}

// blockRange returns the blocks overlapping [start, start+size) of the
// decompressed stream, together with the offset of the range within them.
func (a *unityArchive) blockRange(start, size int64) (first, last int, skip int64) {
	end := start + size
	pos := int64(0)
	first, last = -1, -1
	for i := range a.blocks {
		blockEnd := pos + int64(a.blocks[i].uncompressed)
		if blockEnd > start && pos < end {
			if first < 0 {
				first = i
				skip = start - pos
			}
			last = i
		}
		pos = blockEnd
	}
	if first < 0 {
		return 0, -1, 0
	}
	return first, last, skip
}

// readNode returns a node's bytes, decompressing the blocks it spans.
func (a *unityArchive) readNode(n unityNode) ([]byte, error) {
	if n.size == 0 {
		return nil, nil
	}
	first, last, skip := a.blockRange(n.offset, n.size)
	if last < 0 {
		return nil, fmt.Errorf("node %q has no data blocks", n.path)
	}
	f, err := os.Open(a.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make([]byte, 0, n.size)
	need := n.size
	for i := first; i <= last && need > 0; i++ {
		b := a.blocks[i]
		raw := make([]byte, b.compressed)
		if _, err := f.ReadAt(raw, a.blockOffs[i]); err != nil {
			return nil, err
		}
		block, err := unityDecompress(raw, b.uncompressed, uint32(b.flags)&unityFlagCompressionMask)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", i, err)
		}
		if skip > 0 {
			if skip >= int64(len(block)) {
				skip -= int64(len(block))
				continue
			}
			block = block[skip:]
			skip = 0
		}
		take := int64(len(block))
		if take > need {
			take = need
		}
		out = append(out, block[:take]...)
		need -= take
	}
	if need > 0 {
		return nil, fmt.Errorf("node %q is truncated", n.path)
	}
	return out, nil
}

// unityDecompress decompresses one block or the block information.
func unityDecompress(data []byte, uncompressedSize, compression uint32) ([]byte, error) {
	switch compression {
	case unityCompressionNone:
		if uint32(len(data)) != uncompressedSize {
			return nil, fmt.Errorf("stored size %d does not match %d", len(data), uncompressedSize)
		}
		return data, nil
	case unityCompressionLZ4, unityCompressionLZ4HC:
		out := make([]byte, uncompressedSize)
		n, err := lz4.UncompressBlock(data, out)
		if err != nil {
			return nil, fmt.Errorf("lz4: %w", err)
		}
		return out[:n], nil
	case unityCompressionLZMA:
		// Unity stores raw LZMA1 with a five-byte properties header; the
		// standard reader wants the uncompressed size appended to it.
		if len(data) < 5 {
			return nil, fmt.Errorf("lzma data is truncated")
		}
		var hdr bytes.Buffer
		hdr.Write(data[:5])
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(uncompressedSize))
		hdr.Write(size[:])
		hdr.Write(data[5:])
		r, err := lzma.NewReader(bytes.NewReader(hdr.Bytes()))
		if err != nil {
			return nil, fmt.Errorf("lzma: %w", err)
		}
		out, err := io.ReadAll(io.LimitReader(r, int64(uncompressedSize)+1))
		if err != nil {
			return nil, fmt.Errorf("lzma: %w", err)
		}
		if len(out) != int(uncompressedSize) {
			return nil, fmt.Errorf("lzma produced %d of %d bytes", len(out), uncompressedSize)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported compression %d", compression)
}

// unityMaxStringLen bounds a header string (the version strings are short).
const unityMaxStringLen = 4096

func (a *unityArchive) List() ([]Entry, error) {
	entries := make([]Entry, 0, len(a.nodes))
	for _, n := range a.nodes {
		kind := kindOfFile(n.path)
		entries = append(entries, Entry{
			Name: n.path,
			Size: n.size,
			Kind: kind,
			Mode: defaultMode(kind, 0o644),
		})
	}
	return entries, nil
}

func (a *unityArchive) Open(name string) (io.ReadCloser, int64, error) {
	for _, n := range a.nodes {
		if n.path != name {
			continue
		}
		data, err := a.readNode(n)
		if err != nil {
			return nil, 0, err
		}
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
	}
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}

// unityUsesOldFlagSet reports whether a bundle of this engine version predates
// the flag renumbering (0x200 became padding, encryption moved to 0x1400).
func unityUsesOldFlagSet(engine string) bool {
	major, minor, patch := unityVersionParts(engine)
	switch {
	case major < 2020:
		return true
	case major == 2020:
		return minor < 3 || (minor == 3 && patch < 34)
	case major == 2021:
		return minor < 3 || (minor == 3 && patch < 2)
	case major == 2022:
		return minor < 1
	}
	return false
}

func unityVersionAtLeast(engine string, major, minor, patch int) bool {
	maj, min, pat := unityVersionParts(engine)
	if maj != major {
		return maj > major
	}
	if min != minor {
		return min > minor
	}
	return pat >= patch
}

// unityVersionParts parses "2019.4.15f1" into its three numbers.
func unityVersionParts(engine string) (int, int, int) {
	parts := strings.SplitN(engine, ".", 4)
	if len(parts) < 2 {
		return 0, 0, 0
	}
	atoi := func(s string) int {
		digits := s
		for i, r := range s {
			if r < '0' || r > '9' {
				digits = s[:i]
				break
			}
		}
		v, _ := strconv.Atoi(digits)
		return v
	}
	major, minor := atoi(parts[0]), atoi(parts[1])
	patch := 0
	if len(parts) >= 3 {
		patch = atoi(parts[2])
	}
	return major, minor, patch
}

// kindOfFile classifies a bundle node: directories are rare but legal.
func kindOfFile(path string) fsview.Kind {
	if strings.HasSuffix(path, "/") {
		return fsview.KindDir
	}
	return fsview.KindFile
}

// --- small readers ---

// fileCursor reads sequential fields from a file.
type fileCursor struct {
	f    *os.File
	pos  int64
	size int64
}

func (c *fileCursor) bytes(n int) ([]byte, error) {
	if n < 0 || c.pos+int64(n) > c.size {
		return nil, io.ErrUnexpectedEOF
	}
	buf := make([]byte, n)
	if _, err := c.f.ReadAt(buf, c.pos); err != nil {
		return nil, err
	}
	c.pos += int64(n)
	return buf, nil
}

func (c *fileCursor) u16() (uint16, error) {
	b, err := c.bytes(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

func (c *fileCursor) u32() (uint32, error) {
	b, err := c.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (c *fileCursor) u64() (int64, error) {
	b, err := c.bytes(8)
	if err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}

func (c *fileCursor) cstring(limit int) (string, error) {
	var out []byte
	for len(out) < limit {
		b, err := c.bytes(1)
		if err != nil {
			return "", err
		}
		if b[0] == 0 {
			return string(out), nil
		}
		out = append(out, b[0])
	}
	return "", fmt.Errorf("unterminated string in header")
}

func (c *fileCursor) skip(n int64) error {
	if n < 0 || c.pos+n > c.size {
		return io.ErrUnexpectedEOF
	}
	c.pos += n
	return nil
}

func (c *fileCursor) align(n int64) {
	if rem := c.pos % n; rem != 0 {
		c.pos += n - rem
	}
}

// memCursor reads fields from an in-memory blob.
type memCursor struct {
	b   []byte
	pos int
}

func (c *memCursor) bytes(n int) ([]byte, error) {
	if n < 0 || c.pos+n > len(c.b) {
		return nil, io.ErrUnexpectedEOF
	}
	out := c.b[c.pos : c.pos+n]
	c.pos += n
	return out, nil
}

func (c *memCursor) u16() (uint16, error) {
	b, err := c.bytes(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

func (c *memCursor) u32() (uint32, error) {
	b, err := c.bytes(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (c *memCursor) i64() (int64, error) {
	b, err := c.bytes(8)
	if err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}

func (c *memCursor) cstring(limit int) (string, error) {
	start := c.pos
	for i := 0; i < limit; i++ {
		b, err := c.bytes(1)
		if err != nil {
			return "", err
		}
		if b[0] == 0 {
			return string(c.b[start : start+i]), nil
		}
	}
	return "", fmt.Errorf("unterminated string in bundle information")
}

var _ Archive = (*unityArchive)(nil)
