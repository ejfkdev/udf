// Package archive reads archive / container formats — tar (and compressed tar),
// zip, 7z, rar, cpio, asar, rpm, deb and OCI image layouts — and exposes a
// uniform list/open surface over them. Formats are identified by content, not
// filename extension (see Detect), so a misnamed file still opens correctly.
package archive

import (
	"archive/tar"
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bodgit/sevenzip"
	"github.com/cavaliergopher/cpio"
	"github.com/nwaples/rardecode/v2"

	"github.com/ejfkdev/udf/fsview"
)

// Entry describes one member of an archive.
type Entry struct {
	Name     string
	Size     int64
	Kind     fsview.Kind
	Mode     int64 // permission + setuid/setgid/sticky bits (tar-style low 12 bits)
	ModTime  time.Time
	UID      int
	GID      int
	Uname    string
	Gname    string
	Linkname string
	Devmajor int64 // device major number, only meaningful for device kinds
	Devminor int64 // device minor number, only meaningful for device kinds
}

// Archive is an opened archive: List enumerates its members, Open streams one
// member's contents.
type Archive interface {
	List() ([]Entry, error)
	Open(name string) (io.ReadCloser, int64, error)
}

// Open opens an archive, dispatching on content rather than filename extension.
func Open(path string) (Archive, error) {
	if IsOCILayout(path) {
		return openOCIDir(path)
	}
	format, err := Detect(path)
	if err != nil {
		return nil, err
	}
	switch format {
	case "tar":
		return openTar(path, "")
	case "tar.gz":
		return openTar(path, "gzip")
	case "tar.bz2":
		return openTar(path, "bzip2")
	case "tar.xz":
		return openTar(path, "xz")
	case "tar.zst":
		return openTar(path, "zstd")
	case "tar.lz4":
		return openTar(path, "lz4")
	case "deb":
		return &debArchive{path: path}, nil
	case "zip":
		// .ppkg (Windows provisioning package) is an OPC container: a ZIP.
		return &zipArchive{path: path}, nil
	case "7z":
		return &sevenzipArchive{path: path}, nil
	case "cpio":
		return &cpioArchive{path: path}, nil
	case "cpio.gz":
		return &cpioArchive{path: path, compression: "gzip"}, nil
	case "cpio.bz2":
		return &cpioArchive{path: path, compression: "bzip2"}, nil
	case "cpio.xz":
		return &cpioArchive{path: path, compression: "xz"}, nil
	case "cpio.zst":
		return &cpioArchive{path: path, compression: "zstd"}, nil
	case "cpio.lz4":
		return &cpioArchive{path: path, compression: "lz4"}, nil
	case "rar":
		return &rarArchive{path: path}, nil
	case "asar":
		return &asarArchive{path: path}, nil
	case "rpm":
		return &rpmArchive{path: path}, nil
	case "cab":
		return &cabArchive{path: path}, nil
	case "nar":
		return &narArchive{path: path}, nil
	case "xar":
		return &xarArchive{path: path}, nil
	case "pyinstaller":
		return &pyinstArchive{path: path}, nil
	case "dotnet-bundle":
		return openDotnetBundle(path)
	case "nuitka":
		return &nuitkaArchive{path: path}, nil
	default:
		return nil, fmt.Errorf("unsupported archive format: %s", filepath.Base(path))
	}
}

// openTar opens a tar (optionally compressed), routing an embedded OCI image
// archive (oci-layout + index.json entries) to the OCI reader.
func openTar(path, compression string) (Archive, error) {
	ta := &tarArchive{path: path, compression: compression}
	if ta.hasOCILayoutEntries() {
		return openOCITar(path, compression)
	}
	return ta, nil
}

// hasOCILayoutEntries reports whether the tar holds an OCI image archive by
// scanning only the leading entries (oci-layout and index.json come first; a
// docker-save tar starts with manifest.json), so large layer blobs are never
// decompressed during detection.
func (a *tarArchive) hasOCILayoutEntries() bool {
	tr, closeFn, err := a.newReader()
	if err != nil {
		return false
	}
	defer closeFn()

	index, ociLayout := false, false
	for i := 0; i < 8; i++ {
		hdr, err := tr.Next()
		if err != nil {
			return false
		}
		switch hdr.Name {
		case "index.json":
			index = true
		case "oci-layout":
			ociLayout = true
		case "manifest.json":
			return false // docker save, not OCI
		}
		if index && ociLayout {
			return true
		}
	}
	return false
}

// permSpecial distills the permission plus setuid/setgid/sticky bits of an
// os.FileMode into the low-12-bit form the listing renderer expects.
func permSpecial(m os.FileMode) int64 {
	v := int64(m.Perm())
	if m&os.ModeSetuid != 0 {
		v |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		v |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		v |= 0o1000
	}
	return v
}

func kindFromMode(m os.FileMode) fsview.Kind {
	if m.IsDir() {
		return fsview.KindDir
	}
	if m&os.ModeSymlink != 0 {
		return fsview.KindSymlink
	}
	return fsview.KindFile
}

func tarKind(hdr *tar.Header) fsview.Kind {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return fsview.KindDir
	case tar.TypeSymlink:
		return fsview.KindSymlink
	case tar.TypeLink:
		return fsview.KindHardlink
	case tar.TypeChar:
		return fsview.KindCharDev
	case tar.TypeBlock:
		return fsview.KindBlockDev
	case tar.TypeFifo:
		return fsview.KindFifo
	default:
		return fsview.KindFile
	}
}

func cpioKind(m cpio.FileMode) fsview.Kind {
	switch uint32(m) & 0o170000 {
	case 0o040000:
		return fsview.KindDir
	case 0o120000:
		return fsview.KindSymlink
	case 0o020000:
		return fsview.KindCharDev
	case 0o060000:
		return fsview.KindBlockDev
	case 0o010000:
		return fsview.KindFifo
	default:
		return fsview.KindFile
	}
}

// defaultMode fills in a conventional mode when a format does not record one.
func defaultMode(kind fsview.Kind, mode int64) int64 {
	if mode != 0 {
		return mode
	}
	switch kind {
	case fsview.KindDir:
		return 0o755
	case fsview.KindSymlink:
		return 0o777
	default:
		return 0o644
	}
}

// --- tar ---

type tarArchive struct {
	path        string
	compression string // "", "gzip", "bzip2", "xz", "lzma", "zstd", "lz4"
}

func (a *tarArchive) List() ([]Entry, error) {
	tr, closeFn, err := a.newReader()
	if err != nil {
		return nil, err
	}
	defer closeFn()

	var entries []Entry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read tar entry: %w", err)
		}
		kind := tarKind(hdr)
		entries = append(entries, Entry{
			Name:     hdr.Name,
			Size:     hdr.Size,
			Kind:     kind,
			Mode:     defaultMode(kind, hdr.Mode&0o7777),
			ModTime:  hdr.ModTime,
			UID:      hdr.Uid,
			GID:      hdr.Gid,
			Uname:    hdr.Uname,
			Gname:    hdr.Gname,
			Linkname: hdr.Linkname,
			Devmajor: hdr.Devmajor,
			Devminor: hdr.Devminor,
		})
	}
}

func (a *tarArchive) Open(name string) (io.ReadCloser, int64, error) {
	tr, closeFn, err := a.newReader()
	if err != nil {
		return nil, 0, err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			closeFn()
			return nil, 0, fmt.Errorf("entry %s not found in archive", name)
		}
		if err != nil {
			closeFn()
			return nil, 0, fmt.Errorf("read tar entry: %w", err)
		}
		if hdr.Name != name {
			continue
		}
		return &tarEntryReadCloser{Reader: tr, closeFn: closeFn}, hdr.Size, nil
	}
}

func (a *tarArchive) newReader() (*tar.Reader, func(), error) {
	f, err := os.Open(a.path)
	if err != nil {
		return nil, nil, err
	}
	r, closeDec, err := openCompressed(f, a.compression)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("open %s: %w", a.path, err)
	}
	closeFn := func() {
		closeDec()
		_ = f.Close()
	}
	return tar.NewReader(r), closeFn, nil
}

type tarEntryReadCloser struct {
	io.Reader
	closeFn func()
}

func (r *tarEntryReadCloser) Close() error {
	if r.closeFn != nil {
		r.closeFn()
	}
	return nil
}

// --- zip ---

type zipArchive struct {
	path string
}

func (a *zipArchive) List() ([]Entry, error) {
	zr, err := zip.OpenReader(a.path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	entries := make([]Entry, 0, len(zr.File))
	for _, f := range zr.File {
		kind := kindFromMode(f.Mode())
		linkname := ""
		if kind == fsview.KindSymlink {
			linkname = zipLinkTarget(f)
		}
		size := int64(f.UncompressedSize64)
		if kind == fsview.KindFile && maybeAXMLName(f.Name) {
			// APK XML entries are compiled binary AXML; report the size of
			// the decoded text that listing and extraction will show.
			if decoded, ok := zipDecodeAXML(f); ok {
				size = int64(len(decoded))
			}
		}
		entries = append(entries, Entry{
			Name:     f.Name,
			Size:     size,
			Kind:     kind,
			Mode:     defaultMode(kind, permSpecial(f.Mode())),
			ModTime:  f.Modified,
			Linkname: linkname,
		})
	}
	return entries, nil
}

// maybeAXMLName reports whether the entry name suggests compiled Android XML.
func maybeAXMLName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".xml") || strings.HasSuffix(lower, ".axml")
}

// zipDecodeAXML reads a zip entry and decodes it when it is binary Android
// XML, returning the decoded text.
func zipDecodeAXML(f *zip.File) (string, bool) {
	if f.UncompressedSize64 == 0 || f.UncompressedSize64 > axmlMaxEntrySize {
		return "", false
	}
	rc, err := f.Open()
	if err != nil {
		return "", false
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, int64(axmlMaxEntrySize)+1))
	if err != nil || !isAXML(data) {
		return "", false
	}
	decoded, err := DecodeAXML(data)
	if err != nil {
		return "", false
	}
	return decoded, true
}

// axmlMaxEntrySize bounds how much of an entry is buffered for AXML decoding;
// compiled manifests and layout files stay well below this.
const axmlMaxEntrySize = 8 << 20

// zipLinkTarget reads the link target stored in a ZIP symlink entry's content
// (the Unix "symlink stored as a regular file" convention). It returns "" when
// the entry cannot be read.
func zipLinkTarget(f *zip.File) string {
	rc, err := f.Open()
	if err != nil {
		return ""
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 4096))
	if err != nil {
		return ""
	}
	return string(data)
}

func (a *zipArchive) Open(name string) (io.ReadCloser, int64, error) {
	zr, err := zip.OpenReader(a.path)
	if err != nil {
		return nil, 0, err
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		if maybeAXMLName(f.Name) {
			if decoded, ok := zipDecodeAXML(f); ok {
				_ = zr.Close()
				return io.NopCloser(strings.NewReader(decoded)), int64(len(decoded)), nil
			}
		}
		rc, err := f.Open()
		if err != nil {
			_ = zr.Close()
			return nil, 0, err
		}
		return &zipEntryReadCloser{
			ReadCloser: rc,
			closeZip:   func() { _ = zr.Close() },
		}, int64(f.UncompressedSize64), nil
	}
	_ = zr.Close()
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}

type zipEntryReadCloser struct {
	io.ReadCloser
	closeZip func()
}

func (r *zipEntryReadCloser) Close() error {
	err := r.ReadCloser.Close()
	if r.closeZip != nil {
		r.closeZip()
	}
	return err
}

// --- 7z ---

type sevenzipArchive struct {
	path string
}

func (a *sevenzipArchive) List() ([]Entry, error) {
	zr, err := sevenzip.OpenReader(a.path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	entries := make([]Entry, 0, len(zr.File))
	for _, f := range zr.File {
		mode := f.Mode()
		kind := kindFromMode(mode)
		entries = append(entries, Entry{
			Name:    f.Name,
			Size:    int64(f.UncompressedSize),
			Kind:    kind,
			Mode:    defaultMode(kind, permSpecial(mode)),
			ModTime: f.Modified,
		})
	}
	return entries, nil
}

func (a *sevenzipArchive) Open(name string) (io.ReadCloser, int64, error) {
	zr, err := sevenzip.OpenReader(a.path)
	if err != nil {
		return nil, 0, err
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			_ = zr.Close()
			return nil, 0, err
		}
		return &zipEntryReadCloser{
			ReadCloser: rc,
			closeZip:   func() { _ = zr.Close() },
		}, int64(f.UncompressedSize), nil
	}
	_ = zr.Close()
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}

// --- cpio ---

type cpioArchive struct {
	path        string
	compression string // "", "gzip", "bzip2", "xz", "lzma", "zstd", "lz4"
}

func (a *cpioArchive) newReader() (*cpio.Reader, func(), error) {
	f, err := os.Open(a.path)
	if err != nil {
		return nil, nil, err
	}
	r, closeDec, err := openCompressed(f, a.compression)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("open %s: %w", a.path, err)
	}
	closeFn := func() {
		closeDec()
		_ = f.Close()
	}
	return cpio.NewReader(r), closeFn, nil
}

func (a *cpioArchive) List() ([]Entry, error) {
	cr, closeFn, err := a.newReader()
	if err != nil {
		return nil, err
	}
	defer closeFn()

	var entries []Entry
	for {
		hdr, err := cr.Next()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read cpio entry: %w", err)
		}
		kind := cpioKind(hdr.Mode)
		entries = append(entries, Entry{
			Name:     hdr.Name,
			Size:     hdr.Size,
			Kind:     kind,
			Mode:     defaultMode(kind, int64(uint32(hdr.Mode)&0o7777)),
			ModTime:  hdr.ModTime,
			UID:      hdr.Uid,
			GID:      hdr.Guid,
			Linkname: hdr.Linkname,
		})
	}
}

func (a *cpioArchive) Open(name string) (io.ReadCloser, int64, error) {
	cr, closeFn, err := a.newReader()
	if err != nil {
		return nil, 0, err
	}
	for {
		hdr, err := cr.Next()
		if err == io.EOF {
			closeFn()
			return nil, 0, fmt.Errorf("entry %s not found in archive", name)
		}
		if err != nil {
			closeFn()
			return nil, 0, fmt.Errorf("read cpio entry: %w", err)
		}
		if hdr.Name != name {
			continue
		}
		return &tarEntryReadCloser{Reader: cr, closeFn: closeFn}, hdr.Size, nil
	}
}

// --- rar ---

type rarArchive struct {
	path string
}

func (a *rarArchive) List() ([]Entry, error) {
	files, err := rardecode.List(a.path)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(files))
	for _, f := range files {
		kind := fsview.KindFile
		if f.IsDir {
			kind = fsview.KindDir
		} else if f.LinkTarget != "" {
			kind = fsview.KindSymlink
		}
		entries = append(entries, Entry{
			Name:     f.Name,
			Size:     f.UnPackedSize,
			Kind:     kind,
			Mode:     defaultMode(kind, permSpecial(f.Mode())),
			ModTime:  f.ModificationTime,
			Linkname: f.LinkTarget,
		})
	}
	return entries, nil
}

func (a *rarArchive) Open(name string) (io.ReadCloser, int64, error) {
	files, err := rardecode.List(a.path)
	if err != nil {
		return nil, 0, err
	}
	for _, f := range files {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, 0, err
		}
		return &zipEntryReadCloser{ReadCloser: rc, closeZip: func() {}}, f.UnPackedSize, nil
	}
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}
