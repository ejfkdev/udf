// read.go
package ntfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"unicode/utf16"

	diskfs "github.com/carbon-os/diskimg/fs"
)

// ── run / runlist ─────────────────────────────────────────────────────────────

// run is one contiguous extent in a non-resident attribute's runlist.
type run struct {
	lcn    int64 // logical cluster number (partition-relative); -1 = sparse
	length int64 // cluster count
}

// decodeRunlist parses the compact NTFS runlist encoding from data.
// Each entry begins with a header byte: high nibble = bytes for LCN delta,
// low nibble = bytes for run length.  LCN deltas are signed and relative.
func decodeRunlist(data []byte) ([]run, error) {
	var runs []run
	var prevLCN int64
	i := 0
	for i < len(data) {
		hdr := data[i]
		i++
		if hdr == 0x00 {
			break // end of runlist
		}
		lenBytes := int(hdr & 0x0F)
		offBytes := int(hdr >> 4)
		if i+lenBytes+offBytes > len(data) {
			return nil, fmt.Errorf("runlist: entry at %d overflows buffer", i-1)
		}

		// Run length (unsigned).
		var runLen int64
		for j := 0; j < lenBytes; j++ {
			runLen |= int64(data[i+j]) << (8 * uint(j))
		}
		i += lenBytes

		// LCN delta (signed).
		var lcnDelta int64
		if offBytes > 0 {
			for j := 0; j < offBytes; j++ {
				lcnDelta |= int64(data[i+j]) << (8 * uint(j))
			}
			// Sign-extend from offBytes*8 bits.
			if data[i+offBytes-1]&0x80 != 0 {
				lcnDelta |= -1 << (8 * uint(offBytes))
			}
			prevLCN += lcnDelta
		}
		i += offBytes

		lcn := int64(-1) // sparse run when offBytes == 0
		if offBytes > 0 {
			lcn = prevLCN
		}
		runs = append(runs, run{lcn: lcn, length: runLen})
	}
	return runs, nil
}

// encodeRunlist encodes a slice of runs into the compact NTFS format.
func encodeRunlist(runs []run) []byte {
	var out []byte
	prevLCN := int64(0)
	for _, r := range runs {
		lenBytes := minBytesUnsigned(r.length)
		var offBytes int
		var lcnDelta int64
		if r.lcn >= 0 {
			lcnDelta = r.lcn - prevLCN
			offBytes = minBytesSigned(lcnDelta)
			prevLCN = r.lcn
		}
		hdr := byte(offBytes<<4) | byte(lenBytes)
		out = append(out, hdr)
		for j := 0; j < lenBytes; j++ {
			out = append(out, byte(r.length>>(8*uint(j))))
		}
		for j := 0; j < offBytes; j++ {
			out = append(out, byte(lcnDelta>>(8*uint(j))))
		}
	}
	out = append(out, 0x00) // terminator
	return out
}

func minBytesUnsigned(v int64) int {
	if v == 0 {
		return 1
	}
	n := 0
	for v > 0 {
		n++
		v >>= 8
	}
	return n
}

func minBytesSigned(v int64) int {
	if v == 0 {
		return 1
	}
	n := 1
	tmp := v
	for {
		tmp >>= 8
		if tmp == 0 || tmp == -1 {
			break
		}
		n++
	}
	// Ensure sign bit is unambiguous.
	if v > 0 && (v>>(8*uint(n)-1)) != 0 {
		n++
	}
	if v < 0 && (v>>(8*uint(n)-1)) != -1 {
		n++
	}
	return n
}

// ── attribute helpers ─────────────────────────────────────────────────────────

// findAttr returns the first attribute of the given type (and, if name is
// non-empty, with the given UTF-16LE name) in an MFT record.
// Returns nil if not found.
func findAttr(rec []byte, attrType uint32, name string) []byte {
	if len(rec) < 0x30 {
		return nil
	}
	off := int(binary.LittleEndian.Uint16(rec[0x14:])) // first attribute offset
	for off+8 <= len(rec) {
		t := binary.LittleEndian.Uint32(rec[off:])
		if t == attrEND {
			break
		}
		attrLen := int(binary.LittleEndian.Uint32(rec[off+4:]))
		if attrLen <= 0 || off+attrLen > len(rec) {
			break
		}
		if t == attrType {
			if name == "" {
				return rec[off : off+attrLen]
			}
			nameLen := int(rec[off+9])
			nameOff := int(binary.LittleEndian.Uint16(rec[off+10:]))
			if nameLen > 0 && off+nameOff+nameLen*2 <= len(rec) {
				u16 := make([]uint16, nameLen)
				for i := range u16 {
					u16[i] = binary.LittleEndian.Uint16(rec[off+nameOff+i*2:])
				}
				if string(utf16.Decode(u16)) == name {
					return rec[off : off+attrLen]
				}
			}
		}
		off += attrLen
	}
	return nil
}

// attrValue returns the full value of an attribute from an MFT record.
// For resident attributes the data is sliced directly from the record.
// For non-resident attributes the runlist is decoded and clusters are read.
func (v *ntfsVolume) attrValue(rec, attr []byte) ([]byte, error) {
	if len(attr) < 16 {
		return nil, fmt.Errorf("attribute too short")
	}
	nonResident := attr[8]
	if nonResident == 0 {
		// Resident: value is embedded in the attribute.
		valLen := int(binary.LittleEndian.Uint32(attr[0x10:]))
		valOff := int(binary.LittleEndian.Uint16(attr[0x14:]))
		if valOff+valLen > len(attr) {
			return nil, fmt.Errorf("resident attribute value out of bounds")
		}
		out := make([]byte, valLen)
		copy(out, attr[valOff:])
		return out, nil
	}

	// Non-resident: decode runlist and read clusters.
	if len(attr) < 0x40 {
		return nil, fmt.Errorf("non-resident attribute header truncated")
	}
	dataSize := int64(binary.LittleEndian.Uint64(attr[0x30:]))
	rlOff := int(binary.LittleEndian.Uint16(attr[0x20:]))
	if rlOff >= len(attr) {
		return nil, fmt.Errorf("runlist offset out of bounds")
	}
	runs, err := decodeRunlist(attr[rlOff:])
	if err != nil {
		return nil, err
	}
	return v.readRuns(runs, dataSize)
}

// readRuns reads dataSize bytes from the cluster extents in runs.
func (v *ntfsVolume) readRuns(runs []run, dataSize int64) ([]byte, error) {
	out := make([]byte, dataSize)
	var written int64
	for _, r := range runs {
		if written >= dataSize {
			break
		}
		toRead := r.length * v.clusterSize
		if written+toRead > dataSize {
			toRead = dataSize - written
		}
		if r.lcn < 0 {
			// Sparse run: already zeroed by make().
			written += toRead
			continue
		}
		off := r.lcn * v.clusterSize
		if _, err := v.partRead(out[written:written+toRead], off); err != nil {
			return nil, fmt.Errorf("read cluster run at LCN %d: %w", r.lcn, err)
		}
		written += toRead
	}
	return out, nil
}

// readRunsAt reads up to len(p) bytes from cluster extents starting at byteOffset.
func (v *ntfsVolume) readRunsAt(runs []run, byteOffset int64, p []byte) (int, error) {
	var clusterOff int64
	want := int64(len(p))
	var n int64

	for _, r := range runs {
		runBytes := r.length * v.clusterSize
		if clusterOff+runBytes <= byteOffset {
			clusterOff += runBytes
			continue
		}
		// This run overlaps the requested range.
		intraOff := byteOffset - clusterOff
		if intraOff < 0 {
			intraOff = 0
		}
		canRead := runBytes - intraOff
		if canRead > want-n {
			canRead = want - n
		}
		if r.lcn < 0 {
			// Sparse: zero fill.
			for i := int64(0); i < canRead; i++ {
				p[n+i] = 0
			}
		} else {
			off := r.lcn*v.clusterSize + intraOff
			if _, err := v.partRead(p[n:n+canRead], off); err != nil {
				return int(n), err
			}
		}
		n += canRead
		byteOffset += canRead
		clusterOff += runBytes
		if n >= want {
			break
		}
	}
	return int(n), nil
}

// ── path lookup ───────────────────────────────────────────────────────────────

// lookupPath resolves an absolute path (with forward slashes) to its MFT
// record number and the record bytes.  Returns the root record for "/".
func (v *ntfsVolume) lookupPath(p string) (uint64, []byte, error) {
	p = path.Clean("/" + strings.TrimPrefix(p, "/"))
	if p == "/" {
		rec, err := v.getRecord(recRoot)
		return recRoot, rec, err
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	dirNum := uint64(recRoot)
	var dirRec []byte
	var err error

	for i, part := range parts {
		dirRec, err = v.getRecord(dirNum)
		if err != nil {
			return 0, nil, err
		}
		childNum, err := v.lookupInDir(dirRec, dirNum, part)
		if err != nil {
			return 0, nil, fmt.Errorf("%s: %w", strings.Join(parts[:i+1], "/"), err)
		}
		if i == len(parts)-1 {
			rec, err := v.getRecord(childNum)
			return childNum, rec, err
		}
		dirNum = childNum
	}
	return 0, nil, fmt.Errorf("lookupPath: internal error")
}

// lookupInDir finds the MFT record number for name within the directory
// represented by dirRec.
func (v *ntfsVolume) lookupInDir(dirRec []byte, dirNum uint64, name string) (uint64, error) {
	entries, err := v.readDirEntries(dirRec, dirNum)
	if err != nil {
		return 0, err
	}
	nameLower := strings.ToLower(name)
	for _, de := range entries {
		if strings.ToLower(de.info.Name()) == nameLower {
			return de.mftNum, nil
		}
	}
	return 0, fmt.Errorf("%w", fs.ErrNotExist)
}

// readDirEntries returns all directory entries from a directory MFT record.
// It merges entries from the resident $INDEX_ROOT and, if present, the
// non-resident $INDEX_ALLOCATION B-tree.
func (v *ntfsVolume) readDirEntries(dirRec []byte, dirNum uint64) ([]*ntfsDirEntry, error) {
	idxRoot := findAttr(dirRec, attrINDEX_ROOT, "$I30")
	if idxRoot == nil {
		idxRoot = findAttr(dirRec, attrINDEX_ROOT, "")
	}
	if idxRoot == nil {
		return nil, fmt.Errorf("directory has no $INDEX_ROOT")
	}

	// Resident attribute value starts at valOff within the attribute.
	valLen := int(binary.LittleEndian.Uint32(idxRoot[0x10:]))
	valOff := int(binary.LittleEndian.Uint16(idxRoot[0x14:]))
	if valOff+valLen > len(idxRoot) {
		return nil, fmt.Errorf("$INDEX_ROOT value out of bounds")
	}
	val := idxRoot[valOff : valOff+valLen]

	// val layout: [0-3] attr type, [4-7] collation, [8-11] block size,
	// [12] clusters/block, [13-15] pad, then INDEX_HEADER at offset 16.
	if len(val) < 32 {
		return nil, fmt.Errorf("$INDEX_ROOT value too short")
	}
	var entries []*ntfsDirEntry
	entries = v.collectIndexEntries(val[16:], entries)

	// If the index has overflow into $INDEX_ALLOCATION, walk those blocks.
	idxAlloc := findAttr(dirRec, attrINDEX_ALLOCATION, "$I30")
	if idxAlloc == nil {
		idxAlloc = findAttr(dirRec, attrINDEX_ALLOCATION, "")
	}
	if idxAlloc != nil {
		rlOff := int(binary.LittleEndian.Uint16(idxAlloc[0x20:]))
		if rlOff < len(idxAlloc) {
			runs, err := decodeRunlist(idxAlloc[rlOff:])
			if err == nil {
				totalBytes := int64(binary.LittleEndian.Uint64(idxAlloc[0x30:]))
				entries = v.walkIndexAlloc(runs, totalBytes, entries)
			}
		}
	}
	return entries, nil
}

// collectIndexEntries parses raw INDEX_HEADER data and appends entries.
func (v *ntfsVolume) collectIndexEntries(hdr []byte, out []*ntfsDirEntry) []*ntfsDirEntry {
	if len(hdr) < 16 {
		return out
	}
	entriesOff := int(binary.LittleEndian.Uint32(hdr[0:]))
	indexLen := int(binary.LittleEndian.Uint32(hdr[4:]))
	if entriesOff > len(hdr) || indexLen > len(hdr) {
		return out
	}
	data := hdr[entriesOff:indexLen]

	off := 0
	for off+16 <= len(data) {
		mftRef := binary.LittleEndian.Uint64(data[off:])
		entLen := int(binary.LittleEndian.Uint16(data[off+8:]))
		keyLen := int(binary.LittleEndian.Uint16(data[off+10:]))
		flags := binary.LittleEndian.Uint32(data[off+12:])

		if flags&idxFlagLastEntry != 0 {
			break
		}
		if entLen < 16 || off+entLen > len(data) {
			break
		}

		mftNum := mftRef & 0x0000FFFFFFFFFFFF
		if keyLen >= 66 && off+16+keyLen <= len(data) {
			key := data[off+16 : off+16+keyLen]
			de := parseFileNameKey(key, mftNum)
			if de != nil {
				out = append(out, de)
			}
		}
		off += entLen
	}
	return out
}

// walkIndexAlloc reads index blocks from $INDEX_ALLOCATION and collects entries.
func (v *ntfsVolume) walkIndexAlloc(runs []run, totalBytes int64, out []*ntfsDirEntry) []*ntfsDirEntry {
	blockBuf := make([]byte, v.idxBlockSize)
	var blockOff int64
	for _, r := range runs {
		for c := int64(0); c < r.length; c++ {
			if blockOff >= totalBytes {
				break
			}
			off := (r.lcn + c) * v.clusterSize
			n, err := v.partRead(blockBuf, off)
			if err != nil || n < int(v.idxBlockSize) {
				blockOff += v.idxBlockSize
				continue
			}
			if string(blockBuf[:4]) != "INDX" {
				blockOff += v.idxBlockSize
				continue
			}
			block := make([]byte, v.idxBlockSize)
			copy(block, blockBuf)
			applyUSA(block)
			// INDEX_HEADER starts at offset 0x18 in an INDX block.
			out = v.collectIndexEntries(block[0x18:], out)
			blockOff += v.idxBlockSize
		}
	}
	return out
}

// parseFileNameKey parses a $FILE_NAME attribute body and returns an entry.
// Returns nil for "." and ".." (parent references) and namespace-2 (DOS-only) names.
func parseFileNameKey(key []byte, mftNum uint64) *ntfsDirEntry {
	if len(key) < 66 {
		return nil
	}
	ns := key[65] // namespace: 0=POSIX, 1=Win32, 2=DOS, 3=Win32&DOS
	if ns == 2 {
		return nil // skip DOS-only aliases
	}
	nameLen := int(key[64])
	if 66+nameLen*2 > len(key) {
		return nil
	}
	u16 := make([]uint16, nameLen)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(key[66+i*2:])
	}
	name := string(utf16.Decode(u16))
	if name == "." || name == ".." {
		return nil
	}

	flags := binary.LittleEndian.Uint32(key[56:])
	realSize := int64(binary.LittleEndian.Uint64(key[48:]))
	mtime := filetimeToTime(int64(binary.LittleEndian.Uint64(key[16:])))

	isDir := flags&faDirectory != 0
	mode := fs.FileMode(0644)
	if isDir {
		mode = fs.FileMode(0755) | fs.ModeDir
	}
	info := &ntfsFileInfo{
		name:    name,
		size:    realSize,
		mode:    mode,
		modTime: mtime,
		isDir:   isDir,
	}
	return &ntfsDirEntry{info: info, mftNum: mftNum}
}

// ── Volume read methods ───────────────────────────────────────────────────────

// ReadFile reads and returns the complete contents of the named file.
func (v *ntfsVolume) ReadFile(name string) ([]byte, error) {
	_, rec, err := v.lookupPath(name)
	if err != nil {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: err}
	}
	flags := binary.LittleEndian.Uint16(rec[0x16:])
	if flags&mftFlagDir != 0 {
		return nil, &fs.PathError{Op: "readfile", Path: name, Err: fmt.Errorf("is a directory")}
	}
	dataAttr := findAttr(rec, attrDATA, "")
	if dataAttr == nil {
		return []byte{}, nil // zero-length file
	}
	return v.attrValue(rec, dataAttr)
}

// Open returns an fs.File for the named path.
func (v *ntfsVolume) Open(name string) (fs.File, error) {
	mftNum, rec, err := v.lookupPath(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return v.openRecord(mftNum, rec, name, false)
}

// Create creates or truncates the named file and returns a writable handle.
func (v *ntfsVolume) Create(name string) (*diskfs.File, error) {
	f, err := v.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	return f, err
}

// OpenFile opens the named file with the given flags and permissions.
func (v *ntfsVolume) OpenFile(name string, flag int, perm fs.FileMode) (*diskfs.File, error) {
	mftNum, rec, err := v.lookupPath(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		if flag&os.O_CREATE == 0 {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		// Create the file.
		mftNum, rec, err = v.createFile(name, perm)
		if err != nil {
			return nil, &fs.PathError{Op: "create", Path: name, Err: err}
		}
	} else if flag&os.O_TRUNC != 0 {
		if err := v.truncateFile(mftNum, rec); err != nil {
			return nil, err
		}
		rec, err = v.getRecord(mftNum)
		if err != nil {
			return nil, err
		}
	}
	writable := flag&(os.O_WRONLY|os.O_RDWR) != 0
	nf, err := v.openRecord(mftNum, rec, path.Base(name), writable)
	if err != nil {
		return nil, err
	}
	return diskfs.NewFile(nf.(*ntfsFile)), nil
}

// openRecord builds an ntfsFile for an already-looked-up MFT record.
func (v *ntfsVolume) openRecord(mftNum uint64, rec []byte, name string, writable bool) (fs.File, error) {
	recFlags := binary.LittleEndian.Uint16(rec[0x16:])
	isDir := recFlags&mftFlagDir != 0

	fi, err := v.recordInfo(mftNum, rec, name)
	if err != nil {
		return nil, err
	}

	nf := &ntfsFile{
		v:        v,
		mftNum:   mftNum,
		info:     fi,
		writable: writable,
		isDir:    isDir,
	}
	if !isDir {
		dataAttr := findAttr(rec, attrDATA, "")
		if dataAttr != nil && dataAttr[8] == 0 {
			// Resident: load data into buffer.
			nf.resData, _ = v.attrValue(rec, dataAttr)
			nf.size = int64(len(nf.resData))
		} else if dataAttr != nil {
			// Non-resident: decode runlist for streaming.
			rlOff := int(binary.LittleEndian.Uint16(dataAttr[0x20:]))
			nf.size = int64(binary.LittleEndian.Uint64(dataAttr[0x30:]))
			if rlOff < len(dataAttr) {
				nf.runs, _ = decodeRunlist(dataAttr[rlOff:])
			}
		}
	}
	return nf, nil
}

// ReadDir returns the directory entries for the named directory.
func (v *ntfsVolume) ReadDir(name string) ([]fs.DirEntry, error) {
	_, rec, err := v.lookupPath(name)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	flags := binary.LittleEndian.Uint16(rec[0x16:])
	if flags&mftFlagDir == 0 {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fmt.Errorf("not a directory")}
	}
	mftNum, _, _ := v.lookupPath(name)
	entries, err := v.readDirEntries(rec, mftNum)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, len(entries))
	for i, e := range entries {
		out[i] = e
	}
	return out, nil
}

// ── ntfsFile ──────────────────────────────────────────────────────────────────

// ntfsFile is an open file or directory handle, implementing both fs.File
// and diskfs.fileBackend.
type ntfsFile struct {
	v        *ntfsVolume
	mftNum   uint64
	info     *ntfsFileInfo
	runs     []run  // non-nil only for non-resident $DATA
	resData  []byte // non-nil only for resident $DATA
	size     int64
	pos      int64
	writable bool
	isDir    bool

	// writeBuf collects new data for WriteFile-like semantics.
	writeBuf []byte
}

func (f *ntfsFile) Stat() (fs.FileInfo, error) { return f.info, nil }

func (f *ntfsFile) Close() error {
	if f.writable && len(f.writeBuf) > 0 {
		return f.v.flushFileWrite(f)
	}
	return nil
}

func (f *ntfsFile) Read(p []byte) (int, error) {
	if f.isDir {
		return 0, fmt.Errorf("read: is a directory")
	}
	if f.pos >= f.size {
		return 0, io.EOF
	}
	remaining := f.size - f.pos
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	var n int
	var err error
	if f.runs != nil {
		n, err = f.v.readRunsAt(f.runs, f.pos, p)
	} else {
		end := f.pos + int64(len(p))
		if end > int64(len(f.resData)) {
			end = int64(len(f.resData))
		}
		n = copy(p, f.resData[f.pos:end])
	}
	f.pos += int64(n)
	if f.pos >= f.size && err == nil {
		return n, io.EOF
	}
	return n, err
}

func (f *ntfsFile) Write(p []byte) (int, error) {
	if !f.writable {
		return 0, fmt.Errorf("write: file not opened for writing")
	}
	// Buffer writes; committed on Close().
	needed := f.pos + int64(len(p))
	if int64(len(f.writeBuf)) < needed {
		nb := make([]byte, needed)
		copy(nb, f.writeBuf)
		f.writeBuf = nb
	}
	copy(f.writeBuf[f.pos:], p)
	f.pos += int64(len(p))
	if f.pos > f.size {
		f.size = f.pos
	}
	return len(p), nil
}

func (f *ntfsFile) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = f.pos + offset
	case io.SeekEnd:
		newPos = f.size + offset
	default:
		return 0, fmt.Errorf("seek: invalid whence %d", whence)
	}
	if newPos < 0 {
		return 0, fmt.Errorf("seek: negative position")
	}
	f.pos = newPos
	return f.pos, nil
}

func (f *ntfsFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if !f.isDir {
		return nil, fmt.Errorf("readdir: not a directory")
	}
	rec, err := f.v.getRecord(f.mftNum)
	if err != nil {
		return nil, err
	}
	entries, err := f.v.readDirEntries(rec, f.mftNum)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, e)
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}
