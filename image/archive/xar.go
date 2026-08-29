package archive

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"os"

	"github.com/ejfkdev/udf/fsview"
)

// xar support — the archive format used by macOS installer packages (.pkg,
// built by pkgbuild/productbuild). Layout (all multi-byte fields big-endian):
//
//	0..4   magic "xar!"
//	4..6   header size (>= 28)
//	6..8   version (1)
//	8..16  compressed TOC length
//	16..24 uncompressed TOC length
//	24..28 checksum algorithm
//
// The TOC is a zlib-compressed XML document describing the file tree; each
// regular file's <data> gives a heap offset (relative to the end of the
// header+TOC), a compressed <length>, an uncompressed <size> and an
// <encoding> whose style selects gzip/bzip2/lzma/xz or none.

const xarMagic = "xar!"

type xarData struct {
	Length   int64        `xml:"length"`
	Offset   int64        `xml:"offset"`
	Size     int64        `xml:"size"`
	Encoding *xarEncoding `xml:"encoding"`
}

type xarEncoding struct {
	Style string `xml:"style,attr"`
}

type xarFile struct {
	Name  string    `xml:"name"`
	Type  string    `xml:"type"`
	Link  string    `xml:"link"`
	Data  *xarData  `xml:"data"`
	Files []xarFile `xml:"file"`
}

type xarTocBody struct {
	Files []xarFile `xml:"file"`
}

type xarToc struct {
	TOC xarTocBody `xml:"toc"`
}

type xarEntry struct {
	path        string
	kind        fsview.Kind
	size        int64
	link        string
	offset      int64 // heap offset (relative to heap base)
	length      int64 // compressed length
	compression string
}

// loadXAR reads the header, decompresses the TOC and walks it into a flat
// entry list plus a path index, returning also the heap base offset.
func loadXAR(path string) ([]xarEntry, map[string]int, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, 0, err
	}
	if len(data) < 28 || string(data[:4]) != xarMagic {
		return nil, nil, 0, fmt.Errorf("not a xar archive")
	}
	headerSize := int64(binary.BigEndian.Uint16(data[4:6]))
	tocLen := int64(binary.BigEndian.Uint64(data[8:16]))
	if headerSize < 28 || headerSize+tocLen > int64(len(data)) {
		return nil, nil, 0, fmt.Errorf("invalid xar header")
	}

	tocStart := headerSize
	tocEnd := tocStart + tocLen
	zr, err := zlib.NewReader(bytes.NewReader(data[tocStart:tocEnd]))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("open xar TOC: %w", err)
	}
	xmlBytes, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("decompress xar TOC: %w", err)
	}

	var toc xarToc
	if err := xml.Unmarshal(xmlBytes, &toc); err != nil {
		return nil, nil, 0, fmt.Errorf("parse xar TOC: %w", err)
	}

	var entries []xarEntry
	index := make(map[string]int)
	var walk func(files []xarFile, dir string) error
	walk = func(files []xarFile, dir string) error {
		for _, f := range files {
			full := f.Name
			if dir != "" {
				full = dir + "/" + f.Name
			}
			switch f.Type {
			case "directory":
				index[full] = len(entries)
				entries = append(entries, xarEntry{path: full, kind: fsview.KindDir})
				if err := walk(f.Files, full); err != nil {
					return err
				}
			case "symlink":
				index[full] = len(entries)
				entries = append(entries, xarEntry{path: full, kind: fsview.KindSymlink, link: f.Link})
			default: // "file" and anything else with a data block
				e := xarEntry{path: full, kind: fsview.KindFile}
				if f.Data != nil {
					e.size = f.Data.Size
					e.offset = f.Data.Offset
					e.length = f.Data.Length
					if f.Data.Encoding != nil {
						e.compression = xarCompression(f.Data.Encoding.Style)
					}
				}
				index[full] = len(entries)
				entries = append(entries, e)
			}
		}
		return nil
	}
	if err := walk(toc.TOC.Files, ""); err != nil {
		return nil, nil, 0, err
	}
	return entries, index, headerSize + tocLen, nil
}

func xarCompression(style string) string {
	switch style {
	case "application/x-gzip":
		// macOS libxar writes a zlib stream (RFC1950) for this style rather
		// than a bare gzip member, so route it to the zlib decoder.
		return "zlib"
	case "application/x-bzip2":
		return "bzip2"
	case "application/x-lzma":
		return "lzma"
	case "application/x-xz":
		return "xz"
	default:
		return ""
	}
}

type xarArchive struct {
	path string
}

func (a *xarArchive) List() ([]Entry, error) {
	entries, _, _, err := loadXAR(a.path)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		mode := int64(0o644)
		switch e.kind {
		case fsview.KindDir:
			mode = 0o755
		case fsview.KindSymlink:
			mode = 0o777
		}
		out = append(out, Entry{Name: e.path, Size: e.size, Kind: e.kind, Mode: mode, Linkname: e.link})
	}
	return out, nil
}

func (a *xarArchive) Open(name string) (io.ReadCloser, int64, error) {
	entries, index, heapBase, err := loadXAR(a.path)
	if err != nil {
		return nil, 0, err
	}
	i, ok := index[name]
	if !ok {
		return nil, 0, fmt.Errorf("entry %s not found in archive", name)
	}
	e := entries[i]
	if e.kind != fsview.KindFile {
		return nil, 0, fmt.Errorf("entry %s is not a regular file", name)
	}

	f, err := os.Open(a.path)
	if err != nil {
		return nil, 0, err
	}
	sr := io.NewSectionReader(f, heapBase+e.offset, e.length)
	r, closeDec, err := openCompressed(sr, e.compression)
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("open xar entry %s: %w", name, err)
	}
	return &xarFileReadCloser{Reader: r, closeFn: func() { closeDec(); _ = f.Close() }}, e.size, nil
}

type xarFileReadCloser struct {
	io.Reader
	closeFn func()
}

func (r *xarFileReadCloser) Close() error {
	if r.closeFn != nil {
		r.closeFn()
	}
	return nil
}
