// Package udffs implements a minimal read-only UDF (Universal Disk Format)
// reader over an io.ReaderAt, targeting the common ECMA-167 layout used by
// optical media and their ISO images: 2048-byte blocks, a single partition,
// File Entries and Extended File Entries with short/long/in-ICB allocation.
//
// It resolves the Anchor Volume Descriptor, walks the Volume Descriptor
// Sequence to find the partition and File Set Descriptor, and reads files and
// directories through File Identifier Descriptors. Names use the OSTA dstring
// compression IDs 8 (8-bit) and 16 (UTF-16 BE).
package udffs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

const blockSize = 2048

// Fixed-volume locations, in 2048-byte blocks.
const (
	vrsBlock  = 16  // Volume Recognition Sequence (BEA01 / NSR02 / NSR03 / TEA01)
	avdpBlock = 256 // first Anchor Volume Descriptor Pointer
)

// Descriptor tag identifiers (ECMA-167).
const (
	tagAVDP = 0x0002
	tagPD   = 0x0005
	tagLVD  = 0x0006
	tagTD   = 0x0008
	tagFSD  = 0x0100
	tagFE   = 0x0105
	tagEFE  = 0x010A
)

// ICB file types (ECMA-167 4/14.6.6).
const (
	ftDirectory = 0x04
	ftRegular   = 0x05
	ftBlock     = 0x06
	ftChar      = 0x07
	ftFIFO      = 0x09
	ftSocket    = 0x0A
	ftSymlink   = 0x0C
)

// Allocation descriptor kinds, held in the low three bits of an ICB's flags.
const (
	adShort = 0x0000
	adLong  = 0x0001
	adInICB = 0x0003

	icbSetuid = 0x0040
	icbSetgid = 0x0080
	icbSticky = 0x0100
)

// File Identifier Descriptor characteristics.
const (
	fidDirectory = 0x02
	fidParent    = 0x08
	fidDeleted   = 0x04
)

// Extent length high bits encode the extent kind (ECMA-167 4/14.14.1.1); a
// recorded-and-allocated extent uses type 0.
const extLenTypeMask = 0xC0000000

// File Entry permission bits (ECMA-167 4/14.9.5).
const (
	permOExec  = 0x00000001
	permOWrite = 0x00000002
	permORead  = 0x00000004
	permGExec  = 0x00000020
	permGWrite = 0x00000040
	permGRead  = 0x00000080
	permUExec  = 0x00000400
	permUWrite = 0x00000800
	permURead  = 0x00001000
)

var le = binary.LittleEndian

// FS is a read-only UDF filesystem. Paths use the io/fs convention: "." is the
// root and nested paths are slash-relative without a leading slash. It is safe
// for concurrent use.
type FS struct {
	ra        io.ReaderAt
	start     int64  // absolute byte offset of the volume start in ra
	partStart uint32 // partition starting location, in 2048-byte blocks
	rootLbn   uint32 // root directory File Entry, in partition logical blocks

	mu       sync.Mutex
	feCache  map[uint32]*fileEntry
	dirCache map[uint32][]*dirNode
}

// Detect reports whether ra contains a UDF filesystem starting at absolute
// offset start.
func Detect(ra io.ReaderAt, start int64) bool {
	var b [blockSize]byte
	if _, err := ra.ReadAt(b[:], start+avdpBlock*blockSize); err != nil {
		return false
	}
	if le.Uint16(b[0:2]) != tagAVDP {
		return false
	}
	if _, err := ra.ReadAt(b[:], start+vrsBlock*blockSize); err != nil {
		return false
	}
	// VRS records lead with a zero type byte, then the 5-byte identifier.
	switch string(b[1:6]) {
	case "BEA01", "NSR02", "NSR03":
		return true
	}
	return false
}

// Open parses the UDF volume starting at absolute byte offset start in ra.
func Open(ra io.ReaderAt, start int64) (*FS, error) {
	f := &FS{
		ra:       ra,
		start:    start,
		feCache:  make(map[uint32]*fileEntry),
		dirCache: make(map[uint32][]*dirNode),
	}

	var avdp [blockSize]byte
	if _, err := ra.ReadAt(avdp[:], start+avdpBlock*blockSize); err != nil {
		return nil, fmt.Errorf("udf: read anchor volume descriptor: %w", err)
	}
	if le.Uint16(avdp[0:2]) != tagAVDP {
		return nil, errors.New("udf: no anchor volume descriptor at block 256")
	}
	mainLen := le.Uint32(avdp[16:])
	mainLoc := le.Uint32(avdp[20:])

	var fsdLbn uint32
	found := false
	for i := uint32(0); i < mainLen/blockSize; i++ {
		var d [blockSize]byte
		if _, err := ra.ReadAt(d[:], start+int64(mainLoc+i)*blockSize); err != nil {
			return nil, fmt.Errorf("udf: read volume descriptor: %w", err)
		}
		switch le.Uint16(d[0:2]) {
		case tagPD:
			f.partStart = le.Uint32(d[188:])
		case tagLVD:
			fsdLbn = le.Uint32(d[252:]) // logical volume contents use, extLocation
			found = true
		case tagTD:
			i = mainLen // stop at the terminating descriptor
		}
	}
	if !found {
		return nil, errors.New("udf: no logical volume descriptor")
	}

	fsd, err := f.readBlock(f.partStart + fsdLbn)
	if err != nil {
		return nil, err
	}
	if le.Uint16(fsd[0:2]) != tagFSD {
		return nil, errors.New("udf: file set descriptor not found")
	}
	f.rootLbn = le.Uint32(fsd[404:]) // root directory ICB extLocation
	return f, nil
}

// ReadDir lists the immediate children of a directory.
func (f *FS) ReadDir(path string) ([]fs.DirEntry, error) {
	fe, lbn, err := f.resolve(path)
	if err != nil {
		return nil, err
	}
	if fe.fileType != ftDirectory {
		return nil, fmt.Errorf("udf: %s is not a directory", path)
	}
	nodes, err := f.listDir(fe, lbn)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, len(nodes))
	for i, n := range nodes {
		out[i] = n
	}
	return out, nil
}

// Stat returns metadata for path using lstat semantics (symlinks are not
// followed).
func (f *FS) Stat(path string) (fs.FileInfo, error) {
	fe, _, err := f.resolve(path)
	if err != nil {
		return nil, err
	}
	return &fileInfo{fe: fe, name: baseName(path)}, nil
}

// Open opens a path for reading.
func (f *FS) Open(path string) (fs.File, error) {
	fe, _, err := f.resolve(path)
	if err != nil {
		return nil, err
	}
	if fe.fileType != ftRegular {
		return nil, fmt.Errorf("udf: %s is not a regular file", path)
	}
	return &file{f: f, fe: fe, name: baseName(path)}, nil
}

// Readlink returns the target of a symlink.
func (f *FS) Readlink(path string) (string, error) {
	fe, _, err := f.resolve(path)
	if err != nil {
		return "", err
	}
	if fe.fileType != ftSymlink {
		return "", fmt.Errorf("udf: %s is not a symlink", path)
	}
	data, err := f.data(fe)
	if err != nil {
		return "", err
	}
	return decodeSymlink(data), nil
}

// Close releases no resources.
func (f *FS) Close() error { return nil }

// resolve walks path from the root and returns the target File Entry together
// with its partition lbn (used as a directory-cache key).
func (f *FS) resolve(path string) (*fileEntry, uint32, error) {
	rel := strings.Trim(path, "/")
	lbn := f.rootLbn
	fe, err := f.readFE(lbn)
	if err != nil {
		return nil, 0, err
	}
	if rel == "" || rel == "." {
		return fe, lbn, nil
	}
	for _, comp := range strings.Split(rel, "/") {
		if comp == "" || comp == "." {
			continue
		}
		if fe.fileType != ftDirectory {
			return nil, 0, fmt.Errorf("udf: %s is not a directory", comp)
		}
		nodes, err := f.listDir(fe, lbn)
		if err != nil {
			return nil, 0, err
		}
		var child *dirNode
		for _, n := range nodes {
			if n.name == comp {
				child = n
				break
			}
		}
		if child == nil {
			return nil, 0, fs.ErrNotExist
		}
		lbn = child.lbn
		fe, err = f.readFE(lbn)
		if err != nil {
			return nil, 0, err
		}
	}
	return fe, lbn, nil
}

// readFE parses (and caches) the File Entry at a partition lbn.
func (f *FS) readFE(lbn uint32) (*fileEntry, error) {
	f.mu.Lock()
	if fe, ok := f.feCache[lbn]; ok {
		f.mu.Unlock()
		return fe, nil
	}
	f.mu.Unlock()

	b, err := f.partBlock(lbn)
	if err != nil {
		return nil, err
	}
	fe, ok := parseFE(b)
	if !ok {
		return nil, errors.New("udf: bad file entry")
	}

	f.mu.Lock()
	if cached, ok := f.feCache[lbn]; ok {
		f.mu.Unlock()
		return cached, nil
	}
	f.feCache[lbn] = fe
	f.mu.Unlock()
	return fe, nil
}

// listDir parses (and caches) the immediate children of a directory, keyed by
// its File Entry lbn.
func (f *FS) listDir(fe *fileEntry, lbn uint32) ([]*dirNode, error) {
	f.mu.Lock()
	if nodes, ok := f.dirCache[lbn]; ok {
		f.mu.Unlock()
		return nodes, nil
	}
	f.mu.Unlock()

	data, err := f.data(fe)
	if err != nil {
		return nil, err
	}
	var nodes []*dirNode
	for off := 0; off+38 <= len(data); {
		chars := data[off+18]
		lenFI := int(data[off+19])
		icbLbn := le.Uint32(data[off+24:])
		lenImpUse := int(le.Uint16(data[off+36:]))
		nameOff := off + 38 + lenImpUse
		fidLen := 38 + lenImpUse + lenFI
		if pad := fidLen % 4; pad != 0 {
			fidLen += 4 - pad
		}
		if nameOff+lenFI > len(data) {
			break
		}
		if chars&fidParent == 0 && chars&fidDeleted == 0 && lenFI > 0 {
			nodes = append(nodes, &dirNode{
				f:     f,
				name:  decodeName(data[nameOff : nameOff+lenFI]),
				lbn:   icbLbn,
				isDir: chars&fidDirectory != 0,
			})
		}
		off += fidLen
	}

	f.mu.Lock()
	if cached, ok := f.dirCache[lbn]; ok {
		f.mu.Unlock()
		return cached, nil
	}
	f.dirCache[lbn] = nodes
	f.mu.Unlock()
	return nodes, nil
}

// readBlock reads a physical 2048-byte block at an absolute volume block number
// (partition start already folded in).
func (f *FS) readBlock(abs uint32) ([]byte, error) {
	b := make([]byte, blockSize)
	if _, err := f.ra.ReadAt(b, f.start+int64(abs)*blockSize); err != nil && err != io.EOF {
		return nil, err
	}
	return b, nil
}

// partBlock reads a block addressed by a partition logical block number.
func (f *FS) partBlock(lbn uint32) ([]byte, error) { return f.readBlock(f.partStart + lbn) }

// data materialises an entry's full content (directories, symlinks, in-ICB
// files).
func (f *FS) data(fe *fileEntry) ([]byte, error) {
	if fe.flags&0x7 == adInICB {
		return append([]byte(nil), fe.ad...), nil
	}
	var out []byte
	for _, e := range fe.extents() {
		buf := make([]byte, (int64(e.length)+blockSize-1)/blockSize*blockSize)
		if _, err := f.ra.ReadAt(buf, f.start+int64(f.partStart+e.lbn)*blockSize); err != nil && err != io.EOF {
			return nil, err
		}
		out = append(out, buf[:e.length]...)
	}
	return out, nil
}

// readFileAt reads a regular file's data at offset off into p.
func (f *FS) readFileAt(fe *fileEntry, p []byte, off int64) (int, error) {
	size := int64(fe.infoLen)
	if off < 0 {
		return 0, errors.New("udf: negative offset")
	}
	if off >= size {
		return 0, io.EOF
	}
	if want := int64(len(p)); want > size-off {
		p = p[:size-off]
	}

	if fe.flags&0x7 == adInICB {
		return copy(p, fe.ad[off:]), nil
	}

	total := 0
	var base int64
	for _, e := range fe.extents() {
		elen := int64(e.length)
		if off < base+elen {
			within := off - base
			chunk := elen - within
			if rem := int64(len(p)) - int64(total); chunk > rem {
				chunk = rem
			}
			src := f.start + int64(f.partStart+e.lbn)*blockSize + within
			m, err := f.ra.ReadAt(p[total:total+int(chunk)], src)
			total += m
			if err != nil && err != io.EOF {
				return total, err
			}
			off += int64(m)
		}
		base += elen
		if total >= len(p) {
			break
		}
	}
	if total == 0 {
		return 0, io.EOF
	}
	return total, nil
}

// fileEntry is a parsed File Entry or Extended File Entry.
type fileEntry struct {
	fileType uint8
	flags    uint16
	perm     uint32
	uid, gid uint32
	nlink    uint16
	infoLen  uint64
	mtime    time.Time
	ea, ad   []byte
}

// extent is one allocation extent: a partition lbn and byte length.
type extent struct {
	lbn    uint32
	length uint32
}

func parseFE(b []byte) (*fileEntry, bool) {
	ident := le.Uint16(b[0:2])
	if ident != tagFE && ident != tagEFE {
		return nil, false
	}
	fe := &fileEntry{
		fileType: b[16+11],
		flags:    le.Uint16(b[16+18:]),
		uid:      le.Uint32(b[36:]),
		gid:      le.Uint32(b[40:]),
		perm:     le.Uint32(b[44:]),
		nlink:    le.Uint16(b[48:]),
		infoLen:  le.Uint64(b[56:]),
	}

	var mtimeOff, lenEAOff, header int
	if ident == tagEFE {
		mtimeOff, lenEAOff, header = 92, 208, 216
	} else {
		mtimeOff, lenEAOff, header = 84, 168, 176
	}
	fe.mtime = readTimestamp(b[mtimeOff:])
	lenEA := int(le.Uint32(b[lenEAOff:]))
	lenAD := int(le.Uint32(b[lenEAOff+4:]))
	if header+lenEA+lenAD > len(b) {
		return nil, false
	}
	fe.ea = b[header : header+lenEA]
	fe.ad = b[header+lenEA : header+lenEA+lenAD]
	return fe, true
}

func (fe *fileEntry) mode() fs.FileMode {
	var m fs.FileMode
	set := func(bit uint32, mode fs.FileMode) {
		if fe.perm&bit != 0 {
			m |= mode
		}
	}
	set(permURead, 0o400)
	set(permUWrite, 0o200)
	set(permUExec, 0o100)
	set(permGRead, 0o040)
	set(permGWrite, 0o020)
	set(permGExec, 0o010)
	set(permORead, 0o004)
	set(permOWrite, 0o002)
	set(permOExec, 0o001)
	if fe.flags&icbSetuid != 0 {
		m |= fs.ModeSetuid
	}
	if fe.flags&icbSetgid != 0 {
		m |= fs.ModeSetgid
	}
	if fe.flags&icbSticky != 0 {
		m |= fs.ModeSticky
	}
	switch fe.fileType {
	case ftDirectory:
		m |= fs.ModeDir
	case ftSymlink:
		m |= fs.ModeSymlink
	case ftChar:
		m |= fs.ModeCharDevice | fs.ModeDevice
	case ftBlock:
		m |= fs.ModeDevice
	case ftFIFO:
		m |= fs.ModeNamedPipe
	case ftSocket:
		m |= fs.ModeSocket
	}
	return m
}

func (fe *fileEntry) extents() []extent {
	kind := fe.flags & 0x7
	var out []extent
	switch kind {
	case adShort:
		for off := 0; off+8 <= len(fe.ad); off += 8 {
			length := le.Uint32(fe.ad[off:]) &^ extLenTypeMask
			pos := le.Uint32(fe.ad[off+4:])
			if length == 0 {
				continue
			}
			out = append(out, extent{lbn: pos, length: length})
		}
	case adLong:
		for off := 0; off+16 <= len(fe.ad); off += 16 {
			length := le.Uint32(fe.ad[off:]) &^ extLenTypeMask
			pos := le.Uint32(fe.ad[off+4:])
			if length == 0 {
				continue
			}
			out = append(out, extent{lbn: pos, length: length})
		}
	}
	return out
}

// dirNode implements fs.DirEntry for a directory listing.
type dirNode struct {
	f     *FS
	name  string
	lbn   uint32
	isDir bool
}

func (n *dirNode) Name() string { return n.name }
func (n *dirNode) IsDir() bool  { return n.isDir }
func (n *dirNode) Type() fs.FileMode {
	if n.isDir {
		return fs.ModeDir
	}
	return 0
}

// Info resolves the child and returns its FileInfo; used rarely but kept for
// completeness.
func (n *dirNode) Info() (fs.FileInfo, error) {
	fe, err := n.f.readFE(n.lbn)
	if err != nil {
		return nil, err
	}
	return &fileInfo{fe: fe, name: n.name}, nil
}

// fileInfo implements fs.FileInfo for a File Entry.
type fileInfo struct {
	fe   *fileEntry
	name string
}

func (i *fileInfo) Name() string       { return i.name }
func (i *fileInfo) Size() int64        { return int64(i.fe.infoLen) }
func (i *fileInfo) Mode() fs.FileMode  { return i.fe.mode() }
func (i *fileInfo) ModTime() time.Time { return i.fe.mtime }
func (i *fileInfo) IsDir() bool        { return i.fe.mode().IsDir() }
func (i *fileInfo) Sys() any           { return nil }

// file implements fs.File for a regular file, backed by the extent reader.
type file struct {
	f    *FS
	fe   *fileEntry
	name string
	off  int64
}

func (r *file) Stat() (fs.FileInfo, error) {
	return &fileInfo{fe: r.fe, name: r.name}, nil
}
func (r *file) Read(p []byte) (int, error) {
	n, err := r.f.readFileAt(r.fe, p, r.off)
	r.off += int64(n)
	return n, err
}
func (r *file) Close() error { return nil }

func readTimestamp(b []byte) time.Time {
	year := int(le.Uint16(b[2:]))
	if year == 0 {
		return time.Time{}
	}
	return time.Date(year, time.Month(b[4]), int(b[5]), int(b[6]), int(b[7]), int(b[8]),
		int(b[9])*10_000_000, time.UTC)
}

func decodeName(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	switch b[0] {
	case 16:
		u := make([]uint16, 0, (len(b)-1)/2)
		for i := 1; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		}
		return string(utf16.Decode(u))
	default: // 8 (Latin-1)
		var sb strings.Builder
		for _, c := range b[1:] {
			sb.WriteRune(rune(c))
		}
		return sb.String()
	}
}

func decodeSymlink(b []byte) string {
	var parts []string
	abs := false
	for i := 0; i+4 <= len(b); {
		typ := b[i]
		clen := int(b[i+1])
		name := ""
		if clen > 0 && i+4+clen <= len(b) {
			name = decodeName(b[i+4 : i+4+clen])
		}
		switch typ {
		case 1, 2:
			abs = true
		case 3:
			parts = append(parts, "..")
		case 4:
			parts = append(parts, ".")
		case 5:
			parts = append(parts, name)
		}
		i += 4 + clen
	}
	s := strings.Join(parts, "/")
	if abs {
		s = "/" + s
	}
	return s
}

func baseName(path string) string {
	path = strings.Trim(path, "/")
	if path == "" || path == "." {
		return "."
	}
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
