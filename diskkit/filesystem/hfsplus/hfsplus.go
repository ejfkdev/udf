package hfsplus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// The volume header sits 1024 bytes into the volume; all multi-byte fields are
// big-endian. Offsets within it were verified against hdiutil-written images.
const (
	volumeHeaderOffset = 1024
	volumeHeaderSize   = 512
	minBlockSize       = 512
	maxBlockSize       = 1 << 20

	// File type bits of a BSD mode (HFS+ stores unix modes with type bits).
	modeDir     = 0o040000
	modeSymlink = 0o120000
	modeReg     = 0o100000
	modeMask    = 0o170000

	// HFS+ timestamps are seconds since 1904-01-01 00:00 UTC.
	timestampOffset = 2082844800

	// CNIDs with a fixed meaning.
	rootParentCNID = 1 // the root folder's parent in the catalog
	rootCNID       = 2

	// record types in the catalog B-tree.
	recFolder      = 1
	recFile        = 2
	recFolderTrace = 3
	recFileThread  = 4

	// B-tree node kinds.
	nodeLeaf   = -1
	nodeIndex  = 0
	nodeHeader = 1

	maxNodeSize = 1 << 15 // 32 KiB, the largest HFS+ uses
)

// names the VFS layer hides on macOS; they are filesystem implementation
// details (hard-link inode storage, the journal) and are not listed.
var hiddenNames = map[string]bool{
	"\x00\x00\x00\x00HFS+ Private Data": true,
	".HFS+ Private Directory Data\r":    true,
	".journal":                          true,
	".journal_info_block":               true,
}

// Volume is an opened HFS+ volume.
type Volume struct {
	ra         io.ReaderAt
	start      int64
	size       int64
	blockSize  uint32
	volumeName string

	catalog  btree
	overflow btree // extents-overflow B-tree; zero fork when absent

	overflowExtents []overflowGroup // flattened overflow records, lookup-ready

	// The catalog flattened into one sorted slice (leaf order preserved and
	// re-sorted by parent, then name). Built lazily on first use so opening a
	// volume costs one volume-header read; every listing and lookup is a
	// binary search over the index afterwards.
	records   []record
	indexOnce sync.Once
	indexErr  error
}

// ensureIndex builds the catalog index on first use.
func (v *Volume) ensureIndex() error {
	v.indexOnce.Do(func() {
		v.indexErr = v.buildIndex()
	})
	return v.indexErr
}

// record is one catalog entry worth exposing: a file or a directory. Thread
// records fold into their target (the volume name for the root).
type record struct {
	parent   uint32 // CNID of the parent directory
	name     string // "" for the synthetic root
	cnid     uint32
	dir      bool
	mode     uint32 // unix mode with type bits, from the BSD info
	size     int64  // data fork logical size
	mtime    time.Time
	dataFork forkData
}

// forkData is an HFSPlusForkData: a logical size and up to eight extents.
// Files whose fork needs more extents find the rest in the overflow B-tree.
type forkData struct {
	size    int64
	extents []forkExtent
}

type forkExtent struct {
	start uint32 // volume block
	count uint32 // blocks
}

// btree is one HFS+ B-tree file (catalog or extents overflow).
type btree struct {
	fork     forkData
	nodeSize uint32
	root     uint32
	depth    int
}

// Open reads the volume header at start and builds the catalog index. The
// volume occupies [start, start+size) of ra.
func Open(ra io.ReaderAt, start, size int64) (*Volume, error) {
	var vh [volumeHeaderSize]byte
	if _, err := ra.ReadAt(vh[:], start+volumeHeaderOffset); err != nil {
		return nil, fmt.Errorf("read volume header: %w", err)
	}
	sig := string(vh[0:2])
	if sig != "H+" && sig != "HX" {
		return nil, fmt.Errorf("not an HFS+ volume (signature %q)", sig)
	}
	blockSize := binary.BigEndian.Uint32(vh[40:44])
	if blockSize < minBlockSize || blockSize > maxBlockSize || blockSize%512 != 0 {
		return nil, fmt.Errorf("implausible block size %d", blockSize)
	}
	v := &Volume{
		ra:        ra,
		start:     start,
		size:      size,
		blockSize: blockSize,
	}

	catalog, err := readForkData(vh[272 : 272+80])
	if err != nil {
		return nil, fmt.Errorf("catalog fork: %w", err)
	}
	overflow, err := readForkData(vh[192 : 192+80])
	if err != nil {
		return nil, fmt.Errorf("extents overflow fork: %w", err)
	}

	if v.catalog, err = v.openBTree(catalog, "catalog"); err != nil {
		return nil, err
	}
	// An empty overflow file has a zero-size fork; only open it when present.
	if overflow.size > 0 {
		if v.overflow, err = v.openBTree(overflow, "extents overflow"); err != nil {
			return nil, err
		}
		if err := v.buildOverflowIndex(); err != nil {
			return nil, err
		}
	}

	v.volumeName = v.rootThreadName()
	return v, nil
}

// readForkData parses an HFSPlusForkData: logical size, clump size, total
// blocks, then eight 8-byte extents.
func readForkData(b []byte) (forkData, error) {
	if len(b) < 80 {
		return forkData{}, errors.New("fork data truncated")
	}
	f := forkData{size: int64(binary.BigEndian.Uint64(b[0:8]))}
	for i := 0; i < 8; i++ {
		e := b[16+i*8:]
		start, count := binary.BigEndian.Uint32(e[0:4]), binary.BigEndian.Uint32(e[4:8])
		if count != 0 {
			f.extents = append(f.extents, forkExtent{start: start, count: count})
		}
	}
	return f, nil
}

// openBTree reads a B-tree's header node (node 0). The fields that matter are
// read at their verified offsets; free/used node counts are not needed to
// read the tree and are intentionally skipped.
func (v *Volume) openBTree(f forkData, what string) (btree, error) {
	b := btree{fork: f}
	var head [64]byte
	if err := v.forkReadAt(&f, 0, false, 0, head[:]); err != nil {
		return btree{}, fmt.Errorf("read %s header node: %w", what, err)
	}
	kind := int8(head[8])
	if kind != nodeHeader {
		return btree{}, fmt.Errorf("%s node 0 is kind %d, not a header node", what, kind)
	}
	b.depth = int(binary.BigEndian.Uint16(head[14:16]))
	b.root = binary.BigEndian.Uint32(head[16:20])
	b.nodeSize = uint32(binary.BigEndian.Uint16(head[32:34]))
	if b.nodeSize < 512 || b.nodeSize > maxNodeSize || b.nodeSize&(b.nodeSize-1) != 0 {
		return btree{}, fmt.Errorf("%s has implausible node size %d", what, b.nodeSize)
	}
	if b.root == 0 || b.depth < 1 {
		// An allocated-but-empty B-tree (an extents-overflow file with no
		// records yet) legitimately has root 0 depth 0.
		if b.root == 0 && b.depth == 0 {
			return b, nil
		}
		return btree{}, fmt.Errorf("%s B-tree declares root %d depth %d", what, b.root, b.depth)
	}
	return b, nil
}

// forkReadAt reads from a fork, mapping logical offsets through its extents
// (the inline eight first, then the extents-overflow B-tree when cnid is set).
func (v *Volume) forkReadAt(f *forkData, cnid uint32, resource bool, off int64, p []byte) error {
	if off < 0 || off+int64(len(p)) > f.size {
		return fmt.Errorf("read [%d, %d) outside a %d-byte fork", off, off+int64(len(p)), f.size)
	}
	blockSize := int64(v.blockSize)
	done := 0
	for done < len(p) {
		logical := off + int64(done)
		rel := logical % blockSize
		n := int(blockSize - rel)
		if len(p)-done < n {
			n = len(p) - done
		}
		block := logical / blockSize
		// Extent starts are absolute volume blocks; the fork's logical block
		// number must be translated through the cumulative extent counts.
		var (
			extent forkExtent
			found  bool
		)
		var cum int64
		for _, e := range f.extents {
			if block >= cum && block < cum+int64(e.count) {
				extent = forkExtent{start: e.start + uint32(block-cum), count: 1}
				found = true
				break
			}
			cum += int64(e.count)
		}
		if !found {
			// Extent not in the fork's inline list: look in the overflow file.
			e, err := v.overflowExtent(cnid, resource, block)
			if err != nil {
				return err
			}
			extent = e
			if e.count == 0 {
				clear(p[done : done+n]) // sparse hole
				done += n
				continue
			}
		}
		abs := v.start + int64(extent.start)*blockSize + rel
		if _, err := v.ra.ReadAt(p[done:done+n], abs); err != nil {
			return fmt.Errorf("read block %d: %w", block, err)
		}
		done += n
	}
	return nil
}

// overflowGroup is one record of the extents-overflow B-tree: eight extents
// starting at startBlock of one fork of one file.
type overflowGroup struct {
	cnid       uint32
	resource   bool
	startBlock uint32
	extents    []forkExtent
}

// buildOverflowIndex flattens the overflow B-tree into v.overflow, sorted for
// lookup by (cnid, fork, start block).
func (v *Volume) buildOverflowIndex() error {
	if v.overflow.fork.size == 0 || v.overflow.root == 0 {
		return nil // no overflow B-tree, or an allocated-but-empty one
	}
	node := v.overflow.root
	seen := map[uint32]bool{}
	for {
		if seen[node] {
			return errors.New("extents overflow B-tree has a cycle")
		}
		seen[node] = true
		buf, err := v.btreeNode(&v.overflow, node)
		if err != nil {
			return err
		}
		switch kind := int8(buf[8]); kind {
		case nodeIndex:
			offs, err := recordOffsets(buf, v.overflow.nodeSize)
			if err != nil {
				return err
			}
			child, _, err := indexChild(buf, offs[0])
			if err != nil {
				return err
			}
			node = child
		case nodeLeaf:
			offs := offsOf(buf, v.overflow.nodeSize)
			for i := 0; i+1 < len(offs); i++ {
				keyOff := int(offs[i])
				if keyOff+2 > len(buf) {
					return errors.New("overflow key out of range")
				}
				klen := int(binary.BigEndian.Uint16(buf[keyOff:]))
				if klen < 11 || keyOff+2+klen+64 > len(buf) {
					return errors.New("overflow record truncated")
				}
				cnid := binary.BigEndian.Uint32(buf[keyOff+2:])
				forkType := buf[keyOff+6]
				startBlock := binary.BigEndian.Uint32(buf[keyOff+7:])
				valOff := keyOff + 2 + klen
				g := overflowGroup{cnid: cnid, resource: forkType == 0xFF, startBlock: startBlock}
				for j := 0; j < 8; j++ {
					e := buf[valOff+j*8:]
					start, count := binary.BigEndian.Uint32(e[0:4]), binary.BigEndian.Uint32(e[4:8])
					if count != 0 {
						g.extents = append(g.extents, forkExtent{start: start, count: count})
					}
				}
				v.overflowExtents = append(v.overflowExtents, g)
			}
			next := binary.BigEndian.Uint32(buf[0:4])
			if next == 0 {
				sort.Slice(v.overflowExtents, func(i, j int) bool {
					a, b := v.overflowExtents[i], v.overflowExtents[j]
					if a.cnid != b.cnid {
						return a.cnid < b.cnid
					}
					if a.resource != b.resource {
						return !a.resource
					}
					return a.startBlock < b.startBlock
				})
				return nil
			}
			node = next
		default:
			return fmt.Errorf("unexpected overflow node kind %d", kind)
		}
	}
}

// overflowExtent resolves one block of a fork through the overflow index: the
// matching record is the one with the greatest start block at or below block.
func (v *Volume) overflowExtent(cnid uint32, resource bool, block int64) (forkExtent, error) {
	idx := sort.Search(len(v.overflowExtents), func(i int) bool {
		g := v.overflowExtents[i]
		if g.cnid != cnid {
			return g.cnid > cnid
		}
		if g.resource != resource {
			return g.resource
		}
		return int64(g.startBlock) > block
	})
	// Walk back to the last group of this fork at or below block.
	for idx > 0 {
		g := v.overflowExtents[idx]
		if g.cnid == cnid && g.resource == resource && int64(g.startBlock) <= block {
			rel := block - int64(g.startBlock)
			for _, e := range g.extents {
				if rel < int64(e.count) {
					return forkExtent{start: e.start + uint32(rel), count: 1}, nil
				}
				rel -= int64(e.count)
			}
			return forkExtent{}, fmt.Errorf("overflow record for block %d covers fewer blocks", block)
		}
		idx--
		if v.overflowExtents[idx].cnid != cnid || v.overflowExtents[idx].resource != resource {
			break
		}
	}
	return forkExtent{}, fmt.Errorf("fork of file %d has no extent covering block %d", cnid, block)
}

// buildIndex scans the catalog leaves once and flattens the file/folder
// records into v.records, sorted by parent then name.
func (v *Volume) buildIndex() error {
	node := v.catalog.root
	seen := map[uint32]bool{}
	// Descend to the leftmost leaf: index record 0's child, repeatedly.
	for {
		if seen[node] {
			return errors.New("catalog B-tree has a cycle")
		}
		seen[node] = true
		buf, err := v.btreeNode(&v.catalog, node)
		if err != nil {
			return err
		}
		kind := int8(buf[8])
		switch kind {
		case nodeIndex:
			offs, err := recordOffsets(buf, v.catalog.nodeSize)
			if err != nil {
				return err
			}
			if len(offs) == 0 {
				return errors.New("catalog index node has no records")
			}
			child, _, err := indexChild(buf, offs[0])
			if err != nil {
				return err
			}
			node = child
		case nodeLeaf:
			recs, next, err := v.parseLeaf(buf, offsOf(buf, v.catalog.nodeSize))
			if err != nil {
				return err
			}
			v.records = append(v.records, recs...)
			if next == 0 {
				v.finishIndex()
				return nil
			}
			node = next
		default:
			return fmt.Errorf("unexpected catalog node kind %d during scan", kind)
		}
	}
}

// finishIndex sorts the flattened records by parent then (folded) name so a
// parent's children are one contiguous binary-searchable range.
func (v *Volume) finishIndex() {
	sort.Slice(v.records, func(i, j int) bool {
		if v.records[i].parent != v.records[j].parent {
			return v.records[i].parent < v.records[j].parent
		}
		return strings.ToLower(v.records[i].name) < strings.ToLower(v.records[j].name)
	})
}

func offsOf(buf []byte, nodeSize uint32) []uint16 {
	n := int(binary.BigEndian.Uint16(buf[10:12]))
	offs := make([]uint16, 0, n)
	for i := 0; i < n; i++ {
		offs = append(offs, binary.BigEndian.Uint16(buf[int(nodeSize)-2*(i+1):]))
	}
	return offs
}

func recordOffsets(buf []byte, nodeSize uint32) ([]uint16, error) {
	offs := offsOf(buf, nodeSize)
	if len(offs) == 0 {
		return nil, errors.New("node has no records")
	}
	return offs, nil
}

// btreeNode reads one node of a B-tree.
func (v *Volume) btreeNode(b *btree, n uint32) ([]byte, error) {
	if n == 0 {
		return nil, errors.New("node number 0 is the header node")
	}
	buf := make([]byte, b.nodeSize)
	err := v.forkReadAt(&b.fork, 0, false, int64(n)*int64(b.nodeSize), buf)
	if err != nil {
		return nil, fmt.Errorf("read node %d: %w", n, err)
	}
	return buf, nil
}

// indexChild reads the child node number that terminates an index record: the
// key, then a child node number.
func indexChild(buf []byte, keyOff uint16) (uint32, uint32, error) {
	if int(keyOff)+2 > len(buf) {
		return 0, 0, errors.New("index key out of range")
	}
	klen := int(binary.BigEndian.Uint16(buf[keyOff:]))
	end := int(keyOff) + 2 + klen
	if end+4 > len(buf) {
		return 0, 0, errors.New("index record truncated")
	}
	return binary.BigEndian.Uint32(buf[end:]), uint32(klen), nil
}

// parseLeaf turns one leaf node's records into exposed entries. It returns the
// next leaf in the fLink chain.
func (v *Volume) parseLeaf(buf []byte, offs []uint16) ([]record, uint32, error) {
	var out []record
	for i := 0; i+1 < len(offs); i++ {
		keyOff := int(offs[i])
		if keyOff+2 > len(buf) {
			return nil, 0, errors.New("leaf key out of range")
		}
		klen := int(binary.BigEndian.Uint16(buf[keyOff:]))
		parent := binary.BigEndian.Uint32(buf[keyOff+2:])
		nameLen := int(binary.BigEndian.Uint16(buf[keyOff+6:]))
		if keyOff+8+2*nameLen > len(buf) || 2*nameLen != klen-6 {
			return nil, 0, errors.New("catalog key truncated")
		}
		name := decodeUTF16BE(buf[keyOff+8 : keyOff+8+2*nameLen])
		valOff := keyOff + 2 + klen
		if valOff+2 > len(buf) {
			return nil, 0, errors.New("catalog value out of range")
		}
		recType := int16(binary.BigEndian.Uint16(buf[valOff:]))

		switch recType {
		case recFolder:
			if valOff+12 > len(buf) {
				return nil, 0, errors.New("folder record truncated")
			}
			cnid := binary.BigEndian.Uint32(buf[valOff+8:])
			r := record{
				parent: parent,
				name:   name,
				cnid:   cnid,
				dir:    true,
				mode:   modeDir | uint32(binary.BigEndian.Uint16(buf[valOff+42:])),
			}
			// Dates start right after the file/folder id at +12:
			// createDate, contentModDate, attributeModDate, accessDate,
			// backupDate. The mtime users see is contentModDate.
			r.mtime = hfsTime(buf, valOff+16)
			out = append(out, r)
		case recFile:
			if valOff+88+80 > len(buf) {
				return nil, 0, errors.New("file record truncated")
			}
			cnid := binary.BigEndian.Uint32(buf[valOff+8:])
			dataFork, err := readForkData(buf[valOff+88 : valOff+88+80])
			if err != nil {
				return nil, 0, err
			}
			r := record{
				parent:   parent,
				name:     name,
				cnid:     cnid,
				mode:     uint32(binary.BigEndian.Uint16(buf[valOff+42:])),
				size:     dataFork.size,
				dataFork: dataFork,
			}
			r.mtime = hfsTime(buf, valOff+16)
			out = append(out, r)
		case recFolderTrace, recFileThread:
			// Thread records only matter for the root's volume name, read
			// separately; skip them here.
		default:
			return nil, 0, fmt.Errorf("unknown catalog record type %d", recType)
		}
	}
	next := binary.BigEndian.Uint32(buf[0:4]) // fLink
	return out, next, nil
}

func hfsTime(buf []byte, off int) time.Time {
	t := int64(binary.BigEndian.Uint32(buf[off : off+4]))
	return time.Unix(t-timestampOffset, 0).UTC()
}

func decodeUTF16BE(b []byte) string {
	if len(b)%2 != 0 {
		return ""
	}
	runes := make([]rune, 0, len(b)/2)
	for i := 0; i < len(b); i += 2 {
		runes = append(runes, rune(binary.BigEndian.Uint16(b[i:])))
	}
	return string(runes)
}

// rootThreadName reads the volume name from the root folder's thread record
// (the catalog entry keyed (rootCNID, "")).
func (v *Volume) rootThreadName() string {
	// The thread is not in v.records (they are skipped), so read the catalog
	// leaf that holds (rootCNID, "") directly: it is adjacent to the root's
	// folder record, which is the first record with parent 1.
	for _, node := range v.catalogLeaves() {
		buf, err := v.btreeNode(&v.catalog, node)
		if err != nil {
			return ""
		}
		offs := offsOf(buf, v.catalog.nodeSize)
		for i := 0; i+1 < len(offs); i++ {
			keyOff := int(offs[i])
			klen := int(binary.BigEndian.Uint16(buf[keyOff:]))
			parent := binary.BigEndian.Uint32(buf[keyOff+2:])
			nameLen := int(binary.BigEndian.Uint16(buf[keyOff+6:]))
			if parent != rootCNID || nameLen != 0 || klen != 6 {
				continue
			}
			valOff := keyOff + 2 + klen
			if valOff+10 > len(buf) {
				return ""
			}
			if int16(binary.BigEndian.Uint16(buf[valOff:])) != recFolderTrace {
				return ""
			}
			nlen := int(binary.BigEndian.Uint16(buf[valOff+8:]))
			if valOff+10+2*nlen > len(buf) {
				return ""
			}
			return decodeUTF16BE(buf[valOff+10 : valOff+10+2*nlen])
		}
	}
	return ""
}

// catalogLeaves walks the leaf chain once, returning every leaf node number.
func (v *Volume) catalogLeaves() []uint32 {
	var out []uint32
	node := v.catalog.root
	seen := map[uint32]bool{}
	for {
		if seen[node] {
			return out
		}
		seen[node] = true
		buf, err := v.btreeNode(&v.catalog, node)
		if err != nil {
			return out
		}
		switch int8(buf[8]) {
		case nodeIndex:
			offs, err := recordOffsets(buf, v.catalog.nodeSize)
			if err != nil || len(offs) == 0 {
				return out
			}
			child, _, err := indexChild(buf, offs[0])
			if err != nil {
				return out
			}
			node = child
		case nodeLeaf:
			out = append(out, node)
			next := binary.BigEndian.Uint32(buf[0:4])
			if next == 0 {
				return out
			}
			node = next
		default:
			return out
		}
	}
}

// ---------- path resolution on the flattened index ----------

// childrenOf returns the records whose parent is cnid, skipping hidden ones.
func (v *Volume) childrenOf(cnid uint32) []record {
	i := sort.Search(len(v.records), func(i int) bool {
		return v.records[i].parent >= cnid
	})
	var out []record
	for ; i < len(v.records) && v.records[i].parent == cnid; i++ {
		if !hiddenNames[v.records[i].name] {
			out = append(out, v.records[i])
		}
	}
	return out
}

// find resolves one name inside a directory, exact first, then
// case-insensitively (HFS+ volumes are case-insensitive unless the signature
// is "HX").
func (v *Volume) find(cnid uint32, name string) (record, bool) {
	kids := v.childrenOf(cnid)
	for _, r := range kids {
		if r.name == name {
			return r, true
		}
	}
	for _, r := range kids {
		if strings.EqualFold(r.name, name) {
			return r, true
		}
	}
	return record{}, false
}

// resolve walks a slash path (io/fs convention: "." is the root) to a record.
func (v *Volume) resolve(name string) (record, error) {
	if name == "" || name == "." {
		return v.rootRecord()
	}
	clean := path.Clean("/" + name) // fold "", "./", duplicates
	cur, err := v.rootRecord()
	if err != nil {
		return record{}, err
	}
	for _, comp := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		if comp == "" {
			continue
		}
		if !cur.dir {
			return record{}, fs.ErrNotExist
		}
		next, ok := v.find(cur.cnid, comp)
		if !ok {
			return record{}, fs.ErrNotExist
		}
		cur = next
	}
	return cur, nil
}

// rootRecord synthesizes the root directory record (CNID 2).
func (v *Volume) rootRecord() (record, error) {
	r := record{parent: rootParentCNID, cnid: rootCNID, dir: true, mode: modeDir | 0o755}
	// Prefer the real root folder record (keyed (1, volumeName)) for its mode
	// and mtime.
	for _, cand := range v.childrenOf(rootParentCNID) {
		if cand.dir && cand.cnid == rootCNID {
			return cand, nil
		}
	}
	return r, nil
}

// ---------- the io/fs-style surface disk.go consumes ----------

func (v *Volume) Open(name string) (fs.File, error) {
	if err := v.ensureIndex(); err != nil {
		return nil, err
	}
	r, err := v.resolve(name)
	if err != nil {
		return nil, err
	}
	if r.dir {
		return nil, fmt.Errorf("%s is a directory", name)
	}
	return newFileReader(v, &r.dataFork, r.cnid), nil
}

func (v *Volume) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := v.ensureIndex(); err != nil {
		return nil, err
	}
	r, err := v.resolve(name)
	if err != nil {
		return nil, err
	}
	if !r.dir {
		return nil, fmt.Errorf("%s is not a directory", name)
	}
	kids := v.childrenOf(r.cnid)
	out := make([]fs.DirEntry, 0, len(kids))
	for i := range kids {
		out = append(out, dirEntry{info: fileStatOf(&kids[i])})
	}
	return out, nil
}

// Stat has lstat semantics: a symlink reports itself, not its target.
func (v *Volume) Stat(name string) (fs.FileInfo, error) {
	if err := v.ensureIndex(); err != nil {
		return nil, err
	}
	r, err := v.resolve(name)
	if err != nil {
		return nil, err
	}
	return fileStatOf(&r), nil
}

func (v *Volume) Readlink(name string) (string, error) {
	if err := v.ensureIndex(); err != nil {
		return "", err
	}
	r, err := v.resolve(name)
	if err != nil {
		return "", err
	}
	if r.mode&modeMask != modeSymlink {
		return "", fmt.Errorf("%s is not a symlink", name)
	}
	if r.size > 4096 {
		return "", fmt.Errorf("symlink target of %d bytes is too long", r.size)
	}
	buf := make([]byte, r.size)
	if err := v.forkReadAt(&r.dataFork, r.cnid, false, 0, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func (v *Volume) Close() error { return nil }

// ---------- fs plumbing ----------

func fileStatOf(r *record) fileInfo {
	mode := r.mode
	var m fs.FileMode
	switch mode & modeMask {
	case modeDir:
		m = fs.ModeDir
	case modeSymlink:
		m = fs.ModeSymlink
	default:
		m = 0
	}
	m |= fs.FileMode(mode & 0o777)
	return fileInfo{name: r.name, size: r.size, mode: m, mtime: r.mtime}
}

type fileInfo struct {
	name  string
	size  int64
	mode  fs.FileMode
	mtime time.Time
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return i.mode }
func (i fileInfo) ModTime() time.Time { return i.mtime }
func (i fileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fileInfo) Sys() any           { return nil }

type dirEntry struct{ info fileInfo }

func (e dirEntry) Name() string               { return e.info.Name() }
func (e dirEntry) IsDir() bool                { return e.info.IsDir() }
func (e dirEntry) Type() fs.FileMode          { return e.info.Mode().Type() }
func (e dirEntry) Info() (fs.FileInfo, error) { return e.info, nil }

// fileReader streams a fork with a read-ahead window, so large copies do not
// pay one chunk decompression per small buffer.
type fileReader struct {
	v    *Volume
	fork forkData
	cnid uint32
	off  int64

	buf     []byte
	bufOff  int64 // file offset buf[0] corresponds to
	bufUsed int
}

func newFileReader(v *Volume, f *forkData, cnid uint32) *fileReader {
	return &fileReader{v: v, fork: *f, cnid: cnid, bufOff: -1}
}

const readAheadSize = 1 << 20

func (r *fileReader) fill() error {
	n := int64(readAheadSize)
	if remain := r.fork.size - r.off; remain < n {
		n = remain
	}
	if n <= 0 {
		return io.EOF
	}
	r.buf = make([]byte, n)
	if err := r.v.forkReadAt(&r.fork, r.cnid, false, r.off, r.buf); err != nil {
		return err
	}
	r.bufOff, r.bufUsed = r.off, int(n)
	return nil
}

func (r *fileReader) Read(p []byte) (int, error) {
	if r.off >= r.fork.size {
		return 0, io.EOF
	}
	if r.bufOff < 0 || r.off < r.bufOff || r.off >= r.bufOff+int64(r.bufUsed) {
		if err := r.fill(); err != nil {
			return 0, err
		}
	}
	i := int(r.off - r.bufOff)
	n := copy(p, r.buf[i:r.bufUsed])
	r.off += int64(n)
	return n, nil
}

func (r *fileReader) Close() error { return nil }

func (r *fileReader) Stat() (fs.FileInfo, error) {
	return fileInfo{name: "", size: r.fork.size, mtime: time.Time{}}, nil
}
