package archive

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/ejfkdev/udf/fsview"
)

// .NET single-file applications (PublishSingleFile) append a bundle to the
// native apphost: file contents, then a manifest header, with a 32-byte
// signature embedded in the host whose preceding uint64 holds the manifest's
// absolute offset. Bundle format versions: 1 (.NET Core 3.1), 2 (.NET 5),
// 6 (.NET 6+, adds per-file compressed sizes and deflate compression).

// dotnetBundleSignature is the 32-byte marker embedded in every apphost /
// singlefilehost template.
var dotnetBundleSignature = []byte{
	0x8b, 0x12, 0x02, 0xb9, 0x6a, 0x61, 0x20, 0x38,
	0x72, 0x7b, 0x93, 0x02, 0x14, 0xd7, 0xa0, 0x32,
	0x13, 0xf5, 0xb9, 0xe6, 0xef, 0xae, 0x33, 0x18,
	0xee, 0x3b, 0x2d, 0xce, 0x24, 0xb3, 0x6a, 0xae,
}

// dotnetBundleScanLimit bounds the signature search. The signature lives in
// the apphost, which always comes first in the file; the limit keeps scans of
// unrelated multi-GB files cheap.
const dotnetBundleScanLimit = 16 << 20

// Bundle file type codes (hostpolicy bundle_marker.h FileType).
const (
	dotnetFileTypeUnknown           = byte(0)
	dotnetFileTypeAssembly          = byte(1)
	dotnetFileTypeNative            = byte(2)
	dotnetFileTypeDepsJSON          = byte(3)
	dotnetFileTypeRuntimeConfigJSON = byte(4)
	dotnetFileTypeSymbols           = byte(5)
)

type dotnetBundleFile struct {
	offset         int64
	size           int64
	compressedSize int64 // 0 when the content is stored uncompressed
	fileType       byte
	path           string
}

type dotnetBundle struct {
	majorVersion uint32
	files        []dotnetBundleFile
}

// dotnetFindBundle locates and parses the bundle manifest, returning nil when
// the file carries no valid .NET bundle.
func dotnetFindBundle(f *os.File) *dotnetBundle {
	// The bundle host is always a native binary; the cheap prefix check keeps
	// the signature scan off unrelated files.
	if !hasNativeHostPrefix(f) {
		return nil
	}
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	size := st.Size()
	sigAddr := dotnetFindSignature(f, size)
	if sigAddr < 8 {
		return nil
	}

	var manifestAddrBuf [8]byte
	if _, err := f.ReadAt(manifestAddrBuf[:], sigAddr-8); err != nil {
		return nil
	}
	manifestAddr := int64(binary.LittleEndian.Uint64(manifestAddrBuf[:]))
	if manifestAddr < 0 || manifestAddr >= size {
		return nil
	}

	bundle, err := dotnetParseManifest(f, manifestAddr, size)
	if err != nil {
		return nil
	}
	return bundle
}

// dotnetFindSignature scans the leading part of the file in overlapping
// chunks for the 32-byte bundle signature.
func dotnetFindSignature(f *os.File, size int64) int64 {
	limit := size
	if limit > dotnetBundleScanLimit {
		limit = dotnetBundleScanLimit
	}
	const chunk = 64 << 10
	buf := make([]byte, chunk+len(dotnetBundleSignature)-1)
	for off := int64(0); off < limit; {
		n, err := f.ReadAt(buf, off)
		if n >= len(dotnetBundleSignature) {
			if idx := bytes.Index(buf[:n], dotnetBundleSignature); idx >= 0 {
				return off + int64(idx)
			}
		}
		if err != nil || int64(n) <= int64(len(dotnetBundleSignature)) {
			break
		}
		off += int64(n) - int64(len(dotnetBundleSignature)) + 1
	}
	return -1
}

// dotnetReadString reads a .NET BinaryReader-style string: a 7-bit encoded
// length prefix followed by UTF-8 bytes.
func dotnetReadString(buf []byte, pos *int) (string, error) {
	var length uint64
	shift := uint(0)
	for {
		if *pos >= len(buf) {
			return "", fmt.Errorf("truncated string prefix")
		}
		b := buf[*pos]
		*pos++
		length |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
		if shift > 28 {
			return "", fmt.Errorf("implausible string length prefix")
		}
	}
	if length > uint64(len(buf)-*pos) {
		return "", fmt.Errorf("string length %d exceeds manifest", length)
	}
	s := string(buf[*pos : *pos+int(length)])
	*pos += int(length)
	return s, nil
}

func dotnetParseManifest(f *os.File, addr, fileSize int64) (*dotnetBundle, error) {
	// The manifest is small; read a bounded window and extend once when a
	// large file count needs more.
	window := int64(1 << 20)
	if fileSize-addr < window {
		window = fileSize - addr
	}
	buf := make([]byte, window)
	n, err := f.ReadAt(buf, addr)
	if err != nil && err != io.EOF {
		return nil, err
	}
	buf = buf[:n]
	if n < 12 {
		return nil, fmt.Errorf("manifest too short")
	}

	pos := 0
	major := binary.LittleEndian.Uint32(buf[pos:])
	pos += 4
	minor := binary.LittleEndian.Uint32(buf[pos:])
	pos += 4
	numFiles := int32(binary.LittleEndian.Uint32(buf[pos:]))
	pos += 4
	if major < 1 || major > 6 || numFiles < 0 || numFiles > 1<<20 {
		return nil, fmt.Errorf("implausible bundle header (version %d.%d, %d files)", major, minor, numFiles)
	}
	if _, err := dotnetReadString(buf, &pos); err != nil { // bundle ID
		return nil, err
	}
	if major >= 2 {
		// deps.json (offset,size), runtimeconfig.json (offset,size), flags
		pos += 8 + 8 + 8 + 8 + 8
	}
	if pos > len(buf) {
		return nil, fmt.Errorf("truncated bundle header")
	}

	files := make([]dotnetBundleFile, 0, numFiles)
	for i := int32(0); i < numFiles; i++ {
		entryLen := 8 + 8 + 1
		if major >= 6 {
			entryLen += 8
		}
		if pos+entryLen > len(buf) {
			return nil, fmt.Errorf("truncated bundle file entry %d", i)
		}
		e := dotnetBundleFile{
			offset: int64(binary.LittleEndian.Uint64(buf[pos:])),
			size:   int64(binary.LittleEndian.Uint64(buf[pos+8:])),
		}
		pos += 16
		if major >= 6 {
			e.compressedSize = int64(binary.LittleEndian.Uint64(buf[pos:]))
			pos += 8
		}
		e.fileType = buf[pos]
		pos++
		p, err := dotnetReadString(buf, &pos)
		if err != nil {
			return nil, fmt.Errorf("bundle file %d: %w", i, err)
		}
		e.path = p
		stored := e.size
		if e.compressedSize > 0 {
			stored = e.compressedSize
			// Note: deflate can expand small files, so compressedSize may
			// legitimately exceed size.
		}
		if e.offset < 0 || e.offset+stored > fileSize {
			return nil, fmt.Errorf("bundle file %s out of range", e.path)
		}
		files = append(files, e)
	}

	return &dotnetBundle{majorVersion: major, files: files}, nil
}

// dotnetArchive exposes a .NET single-file bundle as a flat archive.
type dotnetArchive struct {
	path   string
	bundle *dotnetBundle
}

func openDotnetBundle(path string) (*dotnetArchive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	bundle := dotnetFindBundle(f)
	if bundle == nil {
		return nil, fmt.Errorf("no .NET bundle manifest found in %s", path)
	}
	return &dotnetArchive{path: path, bundle: bundle}, nil
}

func isDotnetBundle(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return dotnetFindBundle(f) != nil
}

func (a *dotnetArchive) List() ([]Entry, error) {
	entries := make([]Entry, 0, len(a.bundle.files))
	for _, e := range a.bundle.files {
		mode := int64(0o644)
		if e.fileType == dotnetFileTypeNative {
			mode = 0o755
		}
		entries = append(entries, Entry{
			Name: e.path,
			Size: e.size,
			Kind: fsview.KindFile,
			Mode: mode,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func (a *dotnetArchive) Open(name string) (io.ReadCloser, int64, error) {
	for i := range a.bundle.files {
		e := &a.bundle.files[i]
		if e.path != name {
			continue
		}
		f, err := os.Open(a.path)
		if err != nil {
			return nil, 0, err
		}
		stored := e.size
		if e.compressedSize > 0 {
			stored = e.compressedSize
		}
		sr := io.NewSectionReader(f, e.offset, stored)
		if e.compressedSize > 0 {
			return &dotnetReadCloser{Reader: flate.NewReader(sr), closeFn: func() { _ = f.Close() }}, e.size, nil
		}
		return &dotnetReadCloser{Reader: sr, closeFn: func() { _ = f.Close() }}, e.size, nil
	}
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}

type dotnetReadCloser struct {
	io.Reader
	closeFn func()
}

func (r *dotnetReadCloser) Close() error {
	if r.closeFn != nil {
		r.closeFn()
	}
	return nil
}

var _ Archive = (*dotnetArchive)(nil)
