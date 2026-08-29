// Package erofs implements a read-only reader for uncompressed EROFS
// (Enhanced Read-Only File System) images. It understands both inode forms
// (compact 32-byte and extended 64-byte) and the two uncompressed data layouts
// (FLAT_PLAIN and FLAT_INLINE); compressed inodes are rejected. The layout
// follows the Linux kernel's fs/erofs/erofs_fs.h, as also implemented in the
// MIT-licensed github.com/emmanuel-deloget/fsforge reader.
package erofs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"
)

const (
	superMagic  = 0xE0F5E1E2 // EROFS_SUPER_MAGIC_V1
	superOffset = 1024       // EROFS_SUPER_OFFSET
	blockSize   = 4096       // EROFS block size (PAGE_SIZE)

	inodeCompactSize  = 32
	inodeExtendedSize = 64
	nidSlot           = 32 // inode addressing unit (bytes)

	direntSize = 12 // erofs_dirent: nid(8) nameoff(2) file_type(1) reserved(1)

	// i_format: bit 0 = version (0 compact, 1 extended); bits 1..3 = layout.
	dataLayoutFlatPlain  = 0
	dataLayoutFlatInline = 2
)

var le = binary.LittleEndian

// superblock is the subset of the EROFS v1 superblock the reader uses.
type superblock struct {
	rootNid      uint16
	inos         uint64
	buildTime    uint64
	buildNsec    uint32
	blocks       uint32
	metaBlkaddr  uint32
	xattrBlkaddr uint32
}

// FS is a read-only, lazy EROFS filesystem. Paths use the io/fs convention:
// "." is the root and nested paths are slash-relative without a leading slash.
type FS struct {
	ra    io.ReaderAt
	start int64 // absolute byte offset of the filesystem start

	sb superblock

	mu         sync.Mutex
	inodeCache map[uint64]*inode
	dirCache   map[uint64][]dirent
}

// inode is a parsed inode core plus its data location.
type inode struct {
	mode      fs.FileMode
	size      int64
	union     uint32 // raw block address for data, or rdev for devices
	nlink     uint32
	uid, gid  uint32
	mtime     time.Time
	plainOff  int64
	inlineOff int64
	fullBytes int64
}

// dirent is one directory entry.
type dirent struct {
	name string
	nid  uint64
	typ  uint8
}

// Detect reports whether ra holds an EROFS filesystem starting at start.
func Detect(ra io.ReaderAt, start int64) bool {
	var b [8]byte
	if _, err := ra.ReadAt(b[:], start+superOffset); err != nil {
		return false
	}
	return le.Uint32(b[:]) == superMagic
}

// Open parses the EROFS filesystem starting at absolute byte offset start.
func Open(ra io.ReaderAt, start int64) (*FS, error) {
	var hdr [128]byte
	if _, err := ra.ReadAt(hdr[:], start+superOffset); err != nil {
		return nil, fmt.Errorf("erofs: read superblock: %w", err)
	}
	sb := superblock{
		rootNid:      le.Uint16(hdr[14:]),
		inos:         le.Uint64(hdr[16:]),
		buildTime:    le.Uint64(hdr[24:]),
		buildNsec:    le.Uint32(hdr[32:]),
		blocks:       le.Uint32(hdr[36:]),
		metaBlkaddr:  le.Uint32(hdr[40:]),
		xattrBlkaddr: le.Uint32(hdr[44:]),
	}
	return &FS{
		ra:         ra,
		start:      start,
		sb:         sb,
		inodeCache: make(map[uint64]*inode),
		dirCache:   make(map[uint64][]dirent),
	}, nil
}

// readInode parses (and caches) the inode at nid.
func (f *FS) readInode(nid uint64) (*inode, error) {
	f.mu.Lock()
	if in, ok := f.inodeCache[nid]; ok {
		f.mu.Unlock()
		return in, nil
	}
	f.mu.Unlock()

	off := f.start + int64(f.sb.metaBlkaddr)*blockSize + int64(nid)*nidSlot
	b := make([]byte, inodeExtendedSize)
	if _, err := f.ra.ReadAt(b, off); err != nil && err != io.EOF {
		return nil, err
	}

	format := le.Uint16(b[0:])
	version := int(format & 1)
	layout := int((format >> 1) & 7)
	if layout != dataLayoutFlatPlain && layout != dataLayoutFlatInline {
		return nil, fmt.Errorf("erofs: unsupported data layout %d at nid %d (compressed images cannot be read)", layout, nid)
	}
	icount := le.Uint16(b[2:])

	in := &inode{
		mode:  modeFromUnix(le.Uint16(b[4:])),
		size:  int64(le.Uint64(b[8:])),
		union: le.Uint32(b[16:]),
		uid:   le.Uint32(b[24:]),
		gid:   le.Uint32(b[28:]),
		mtime: time.Unix(int64(le.Uint64(b[32:])), int64(le.Uint32(b[40:]))).UTC(),
	}
	if version == 0 {
		in.nlink = uint32(le.Uint16(b[6:]))
		in.size = int64(le.Uint32(b[8:]))
		in.uid = uint32(le.Uint16(b[24:]))
		in.gid = uint32(le.Uint16(b[26:]))
		in.mtime = time.Unix(int64(f.sb.buildTime), int64(f.sb.buildNsec)).UTC()
	} else {
		in.nlink = le.Uint32(b[44:])
	}

	xbody := xattrIbodySize(icount)
	coreSize := int64(inodeExtendedSize)
	if version == 0 {
		coreSize = inodeCompactSize
	}
	switch layout {
	case dataLayoutFlatPlain:
		in.plainOff = f.start + int64(in.union)*blockSize
		in.fullBytes = in.size
	case dataLayoutFlatInline:
		in.fullBytes = (in.size / blockSize) * blockSize
		in.plainOff = f.start + int64(in.union)*blockSize
		in.inlineOff = off + coreSize + xbody
	}

	f.mu.Lock()
	f.inodeCache[nid] = in
	f.mu.Unlock()
	return in, nil
}

// xattrIbodySize mirrors the kernel's erofs_xattr_ibody_size.
func xattrIbodySize(icount uint16) int64 {
	if icount == 0 {
		return 0
	}
	return int64(12 + (int(icount)-1)*4)
}

// ReadDir lists the immediate children of a directory.
func (f *FS) ReadDir(path string) ([]fs.DirEntry, error) {
	in, nid, err := f.resolve(path)
	if err != nil {
		return nil, err
	}
	if !in.mode.IsDir() {
		return nil, fmt.Errorf("erofs: %s is not a directory", path)
	}
	entries, err := f.listDir(nid, in)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, len(entries))
	for i, d := range entries {
		out[i] = &dirEntry{f: f, name: d.name, nid: d.nid, typ: d.typ}
	}
	return out, nil
}

// Stat returns file info for path with lstat semantics.
func (f *FS) Stat(path string) (fs.FileInfo, error) {
	in, _, err := f.resolve(path)
	if err != nil {
		return nil, err
	}
	return &fileInfo{in: in, name: baseName(path)}, nil
}

// Open opens a regular file for reading.
func (f *FS) Open(path string) (fs.File, error) {
	in, _, err := f.resolve(path)
	if err != nil {
		return nil, err
	}
	if !in.mode.IsRegular() {
		return nil, fmt.Errorf("erofs: %s is not a regular file", path)
	}
	return &file{f: f, in: in, name: baseName(path)}, nil
}

// Readlink returns the target of a symlink.
func (f *FS) Readlink(path string) (string, error) {
	in, _, err := f.resolve(path)
	if err != nil {
		return "", err
	}
	if in.mode&fs.ModeSymlink == 0 {
		return "", fmt.Errorf("erofs: %s is not a symlink", path)
	}
	buf := make([]byte, in.size)
	if _, err := f.readFileAt(in, buf, 0); err != nil {
		return "", err
	}
	return string(buf), nil
}

// Close releases no resources.
func (f *FS) Close() error { return nil }

// resolve walks path from the root nid and returns the target inode + nid.
func (f *FS) resolve(path string) (*inode, uint64, error) {
	rel := strings.Trim(path, "/")
	nid := uint64(f.sb.rootNid)
	in, err := f.readInode(nid)
	if err != nil {
		return nil, 0, err
	}
	if rel == "" || rel == "." {
		return in, nid, nil
	}
	for _, comp := range strings.Split(rel, "/") {
		if comp == "" || comp == "." {
			continue
		}
		if !in.mode.IsDir() {
			return nil, 0, fmt.Errorf("erofs: %s is not a directory", comp)
		}
		entries, err := f.listDir(nid, in)
		if err != nil {
			return nil, 0, err
		}
		var child *dirent
		for i := range entries {
			if entries[i].name == comp {
				child = &entries[i]
				break
			}
		}
		if child == nil {
			return nil, 0, fs.ErrNotExist
		}
		nid = child.nid
		in, err = f.readInode(nid)
		if err != nil {
			return nil, 0, err
		}
	}
	return in, nid, nil
}

// listDir parses (and caches) a directory's entries, keyed by the dir's nid.
func (f *FS) listDir(nid uint64, in *inode) ([]dirent, error) {
	f.mu.Lock()
	if d, ok := f.dirCache[nid]; ok {
		f.mu.Unlock()
		return d, nil
	}
	f.mu.Unlock()

	data := make([]byte, in.size)
	if _, err := f.readFileAt(in, data, 0); err != nil {
		return nil, err
	}

	var out []dirent
	for base := 0; base < len(data); base += blockSize {
		blockLen := blockSize
		if rem := len(data) - base; rem < blockLen {
			blockLen = rem
		}
		block := data[base : base+blockLen]
		if len(block) < direntSize {
			continue
		}
		nameoff0 := int(le.Uint16(block[8:]) & (blockSize - 1))
		if nameoff0 < direntSize || nameoff0 > blockLen {
			continue
		}
		ndir := nameoff0 / direntSize
		for k := 0; k < ndir; k++ {
			d := block[k*direntSize:]
			childNid := le.Uint64(d[0:])
			nameoff := int(le.Uint16(d[8:]) & (blockSize - 1))
			typ := d[10]
			if nameoff < nameoff0 || nameoff > blockLen {
				continue
			}
			nameEnd := blockLen
			if k < ndir-1 {
				nameEnd = int(le.Uint16(block[(k+1)*direntSize+8:]) & (blockSize - 1))
			}
			if nameEnd > blockLen || nameEnd < nameoff {
				nameEnd = blockLen
			}
			name := trimNUL(block[nameoff:nameEnd])
			if name == "." || name == ".." || name == "" {
				continue
			}
			out = append(out, dirent{name: name, nid: childNid, typ: typ})
		}
	}

	f.mu.Lock()
	f.dirCache[nid] = out
	f.mu.Unlock()
	return out, nil
}

// readFileAt reads an inode's data at offset off into p.
func (f *FS) readFileAt(in *inode, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("erofs: negative offset")
	}
	if off >= in.size {
		return 0, io.EOF
	}
	want := int64(len(p))
	if rem := in.size - off; want > rem {
		want = rem
	}
	var n int64
	for n < want {
		at := off + n
		var srcOff, chunk int64
		if at < in.fullBytes {
			srcOff = in.plainOff + at
			chunk = in.fullBytes - at
		} else {
			srcOff = in.inlineOff + (at - in.fullBytes)
			chunk = in.size - at
		}
		if rem := want - n; chunk > rem {
			chunk = rem
		}
		m, err := f.ra.ReadAt(p[n:int64(n)+chunk], srcOff)
		n += int64(m)
		if err != nil && err != io.EOF {
			return int(n), err
		}
		if m == 0 {
			break
		}
	}
	return int(n), nil
}

func trimNUL(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func baseName(p string) string {
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return "."
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// modeFromUnix is the inverse of the kernel's unix→EROFS mode translation.
func modeFromUnix(v uint16) fs.FileMode {
	m := fs.FileMode(v & 0o777)
	if v&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if v&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if v&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	switch v & 0o170000 {
	case 0o040000:
		m |= fs.ModeDir
	case 0o120000:
		m |= fs.ModeSymlink
	case 0o020000:
		m |= fs.ModeCharDevice | fs.ModeDevice
	case 0o060000:
		m |= fs.ModeDevice
	case 0o010000:
		m |= fs.ModeNamedPipe
	case 0o140000:
		m |= fs.ModeSocket
	}
	return m
}

type fileInfo struct {
	in   *inode
	name string
}

func (i *fileInfo) Name() string       { return i.name }
func (i *fileInfo) Size() int64        { return i.in.size }
func (i *fileInfo) Mode() fs.FileMode  { return i.in.mode }
func (i *fileInfo) ModTime() time.Time { return i.in.mtime }
func (i *fileInfo) IsDir() bool        { return i.in.mode.IsDir() }
func (i *fileInfo) Sys() any           { return nil }

type dirEntry struct {
	f    *FS
	name string
	nid  uint64
	typ  uint8
}

func (d *dirEntry) Name() string { return d.name }
func (d *dirEntry) IsDir() bool  { return d.typ == 2 /* FT_DIR */ }
func (d *dirEntry) Type() fs.FileMode {
	if d.typ == 2 {
		return fs.ModeDir
	}
	return 0
}

// Info resolves the child inode and returns its file info.
func (d *dirEntry) Info() (fs.FileInfo, error) {
	in, err := d.f.readInode(d.nid)
	if err != nil {
		return nil, err
	}
	return &fileInfo{in: in, name: d.name}, nil
}

type file struct {
	f    *FS
	in   *inode
	name string
	off  int64
}

func (r *file) Stat() (fs.FileInfo, error) { return &fileInfo{in: r.in, name: r.name}, nil }
func (r *file) Read(p []byte) (int, error) {
	n, err := r.f.readFileAt(r.in, p, r.off)
	r.off += int64(n)
	return n, err
}
func (r *file) Close() error { return nil }
