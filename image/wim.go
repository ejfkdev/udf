package image

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	wimreader "github.com/ejfkdev/udf/wim"
)

// fileAttributeReadOnly is the DOS/Win32 FILE_ATTRIBUTE_READONLY bit, used to
// mark a WIM entry read-only.
const fileAttributeReadOnly = 0x1

// wimFS adapts one image of a WIM file to the diskVolumeReader interface. WIM
// has no Unix permissions or symlinks, so modes are synthesized and Readlink
// always fails. Directory traversal is serialized internally by the vendored
// reader; file-content reads are independent.
type wimFS struct {
	img  *wimreader.Image
	root *wimreader.File
}

// openWIMFS parses the first image of a WIM file backed by be, which for a
// split WIM carries one reader per part.
func openWIMFS(be *diskBackend) (*wimFS, error) {
	var rd *wimreader.Reader
	var err error
	if len(be.parts) > 1 {
		rd, err = wimreader.NewReaderParts(be.parts)
	} else {
		rd, err = wimreader.NewReader(be.ra)
	}
	if err != nil {
		return nil, err
	}
	if len(rd.Image) == 0 {
		return nil, errors.New("wim: no image")
	}
	root, err := rd.Image[0].Open()
	if err != nil {
		return nil, fmt.Errorf("wim: open image: %w", err)
	}
	return &wimFS{img: rd.Image[0], root: root}, nil
}

// openSWMParts opens a split WIM's parts: part 1 is the given path, parts 2+
// follow the wimlib/Windows naming convention "<base>2.swm", "<base>3.swm", …
// It returns the readers in order and a function closing them all.
func openSWMParts(part1 string) ([]io.ReaderAt, func() error, error) {
	f, err := os.Open(part1)
	if err != nil {
		return nil, nil, err
	}
	var hdr [208]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("read swm header %s: %w", part1, err)
	}
	totalParts := int(binary.LittleEndian.Uint16(hdr[42:44]))
	if totalParts < 1 {
		totalParts = 1
	}

	parts := []io.ReaderAt{f}
	closers := []func() error{f.Close}
	closeAll := func() error {
		var first error
		for _, c := range closers {
			if err := c(); err != nil && first == nil {
				first = err
			}
		}
		return first
	}

	ext := filepath.Ext(part1)
	base := strings.TrimSuffix(part1, ext)
	for n := 2; n <= totalParts; n++ {
		path := fmt.Sprintf("%s%d%s", base, n, ext)
		fn, err := os.Open(path)
		if err != nil {
			_ = closeAll()
			return nil, nil, fmt.Errorf("open split part %d (%s): %w", n, path, err)
		}
		parts = append(parts, fn)
		closers = append(closers, fn.Close)
	}
	return parts, closeAll, nil
}

// resolve walks path from the image root and returns the matching entry.
func (w *wimFS) resolve(path string) (*wimreader.File, error) {
	rel := strings.Trim(path, "/")
	cur := w.root
	if rel == "" || rel == "." {
		return cur, nil
	}
	for _, comp := range strings.Split(rel, "/") {
		if comp == "" || comp == "." {
			continue
		}
		if !cur.IsDir() {
			return nil, fmt.Errorf("wim: %s is not a directory", comp)
		}
		children, err := cur.Readdir()
		if err != nil {
			return nil, err
		}
		var next *wimreader.File
		for _, c := range children {
			if c.Name == comp {
				next = c
				break
			}
		}
		if next == nil {
			return nil, fs.ErrNotExist
		}
		cur = next
	}
	return cur, nil
}

func wimMode(f *wimreader.File) os.FileMode {
	if f.IsDir() {
		return os.ModeDir | 0o755
	}
	if f.Attributes&fileAttributeReadOnly != 0 {
		return 0o444
	}
	return 0o644
}

func wimInfo(f *wimreader.File) fs.FileInfo {
	return &wimFileInfo{
		name: f.Name,
		size: f.Size,
		mode: wimMode(f),
		mod:  f.LastWriteTime.Time(),
	}
}

type wimFileInfo struct {
	name string
	size int64
	mode os.FileMode
	mod  time.Time
}

func (i *wimFileInfo) Name() string       { return i.name }
func (i *wimFileInfo) Size() int64        { return i.size }
func (i *wimFileInfo) Mode() fs.FileMode  { return i.mode }
func (i *wimFileInfo) ModTime() time.Time { return i.mod }
func (i *wimFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i *wimFileInfo) Sys() any           { return nil }

type wimDirEntry struct{ f *wimreader.File }

func (d *wimDirEntry) Name() string { return d.f.Name }
func (d *wimDirEntry) IsDir() bool  { return d.f.IsDir() }
func (d *wimDirEntry) Type() fs.FileMode {
	if d.f.IsDir() {
		return fs.ModeDir
	}
	return 0
}
func (d *wimDirEntry) Info() (fs.FileInfo, error) { return wimInfo(d.f), nil }

// Open opens a regular file for reading.
func (w *wimFS) Open(name string) (fs.File, error) {
	f, err := w.resolve(name)
	if err != nil {
		return nil, err
	}
	if f.IsDir() {
		return nil, fmt.Errorf("wim: %s is a directory", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	return &wimFile{ReadCloser: rc, fi: wimInfo(f)}, nil
}

type wimFile struct {
	io.ReadCloser
	fi fs.FileInfo
}

func (f *wimFile) Stat() (fs.FileInfo, error) { return f.fi, nil }

// ReadDir lists the immediate children of a directory.
func (w *wimFS) ReadDir(name string) ([]fs.DirEntry, error) {
	f, err := w.resolve(name)
	if err != nil {
		return nil, err
	}
	if !f.IsDir() {
		return nil, fmt.Errorf("wim: %s is not a directory", name)
	}
	children, err := f.Readdir()
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, 0, len(children))
	for _, c := range children {
		out = append(out, &wimDirEntry{f: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// Stat returns metadata for path with lstat semantics.
func (w *wimFS) Stat(name string) (fs.FileInfo, error) {
	f, err := w.resolve(name)
	if err != nil {
		return nil, err
	}
	return wimInfo(f), nil
}

// Readlink always fails: WIM reparse points are not interpreted.
func (w *wimFS) Readlink(name string) (string, error) {
	return "", fmt.Errorf("wim: %s is not a symlink", name)
}

// Close releases no resources; the owning disk pipeline closes the file.
func (w *wimFS) Close() error { return nil }

// isWIMAt reports whether ra begins with the WIM magic bytes.
func isWIMAt(ra io.ReaderAt) bool {
	var m [8]byte
	if _, err := ra.ReadAt(m[:], 0); err != nil {
		return false
	}
	return string(m[:]) == "MSWIM\x00\x00\x00"
}
