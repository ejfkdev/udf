package archive

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cavaliergopher/cpio"
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
// entry list plus a path index, returning also the heap base offset. Only the
// header and TOC are read — the heap (the file payloads) is fetched on demand
// by Open, so a listing never touches the payload bytes.
func loadXAR(path string) ([]xarEntry, map[string]int, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, err
	}
	defer f.Close()
	var head [28]byte
	if _, err := f.ReadAt(head[:], 0); err != nil {
		return nil, nil, 0, err
	}
	if string(head[0:4]) != xarMagic {
		return nil, nil, 0, fmt.Errorf("not a xar archive")
	}
	headerSize := int64(binary.BigEndian.Uint16(head[4:6]))
	tocLen := int64(binary.BigEndian.Uint64(head[8:16]))
	if headerSize < 28 || tocLen > 64<<20 {
		return nil, nil, 0, fmt.Errorf("invalid xar header")
	}
	st, err := f.Stat()
	if err != nil {
		return nil, nil, 0, err
	}
	if headerSize+tocLen > st.Size() {
		return nil, nil, 0, fmt.Errorf("invalid xar header")
	}

	tocBytes := make([]byte, tocLen)
	if _, err := f.ReadAt(tocBytes, headerSize); err != nil {
		return nil, nil, 0, err
	}
	zr, err := zlib.NewReader(bytes.NewReader(tocBytes))
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

	// Payload and Scripts members are cpio archives wrapped in gzip (or pbzx
	// in Apple's own big packages, which stays unsupported): their contents
	// expand in place as "Payload/<path>", the way an MSI surfaces its CAB
	// members.
	for _, e := range entries {
		if e.kind != fsview.KindFile || !isPkgPayloadName(e.path) {
			continue
		}
		members, closer, err := a.listPkgMember(e)
		if err != nil {
			continue // not a decodable payload: keep it as a plain file
		}
		for _, m := range members {
			m.Name = e.path + "/" + m.Name
			out = append(out, m)
		}
		closer()
	}
	return out, nil
}

// isPkgPayloadName reports whether a member carries an installer's cpio
// payload (flat and nested products both use these names).
func isPkgPayloadName(name string) bool {
	return name == "Payload" || name == "Scripts" ||
		strings.HasSuffix(name, "/Payload") || strings.HasSuffix(name, "/Scripts")
}

// openPkgPayloadStream opens one member's decoded bytes and reports its
// wrapper: gzip (the usual pkgbuild output), pbzx (Apple's chunked wrapper,
// unsupported), or bare cpio.
func (a *xarArchive) openPkgPayloadStream(e xarEntry) (io.Reader, string, func(), error) {
	rc, _, err := a.Open(e.path)
	if err != nil {
		return nil, "", nil, err
	}
	br := newPeekReader(rc)
	magic, _ := br.Peek(4)
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		gz, err := gzip.NewReader(br)
		if err != nil {
			rc.Close()
			return nil, "", nil, err
		}
		return gz, "gzip", func() { gz.Close(); rc.Close() }, nil
	case string(magic) == "pbzx":
		rc.Close()
		return nil, "pbzx", nil, fmt.Errorf("pbzx-wrapped payload is not supported")
	case bytes.HasPrefix(magic, []byte("0707")):
		return br, "cpio", func() { rc.Close() }, nil
	}
	rc.Close()
	return nil, "", nil, fmt.Errorf("not a cpio payload")
}

// cpioStream is the shape shared by the two cpio flavors a pkg payload may
// use: SVR4 newc (initramfs, RPM — via the cpio package) and odc (macOS
// pkgbuild — via cpio_odc.go).
type cpioStream interface {
	Next() (*cpioHeader, error)
	Read(p []byte) (int, error)
}

type cpioHeader struct {
	Name  string
	Mode  uint32
	UID   uint32
	GID   uint32
	Size  int64
	MTime time.Time
}

func openCpioStream(r io.Reader) (cpioStream, error) {
	br := newPeekReader(r)
	magic, err := br.Peek(6)
	if err != nil {
		return nil, err
	}
	switch string(magic) {
	case "070701", "070702":
		return &newcCpioStream{cr: cpio.NewReader(br)}, nil
	case "070707":
		return &odcCpioStream{or: newODCReader(br)}, nil
	}
	return nil, fmt.Errorf("cpio: unknown magic %q", magic)
}

type newcCpioStream struct{ cr *cpio.Reader }

func (n *newcCpioStream) Next() (*cpioHeader, error) {
	hdr, err := n.cr.Next()
	if err != nil {
		return nil, err
	}
	return &cpioHeader{
		Name:  hdr.Name,
		Mode:  uint32(hdr.Mode),
		UID:   uint32(hdr.Uid),
		GID:   uint32(hdr.Guid),
		Size:  hdr.Size,
		MTime: hdr.ModTime,
	}, nil
}

func (n *newcCpioStream) Read(p []byte) (int, error) { return n.cr.Read(p) }

type odcCpioStream struct{ or *odcReader }

func (o *odcCpioStream) Next() (*cpioHeader, error) {
	hdr, err := o.or.Next()
	if err != nil {
		return nil, err
	}
	return &cpioHeader{
		Name:  hdr.Name,
		Mode:  hdr.Mode,
		UID:   hdr.UID,
		GID:   hdr.GID,
		Size:  hdr.Size,
		MTime: hdr.MTime,
	}, nil
}

func (o *odcCpioStream) Read(p []byte) (int, error) { return o.or.Read(p) }

// listPkgCpio walks one cpio stream into entries.
func listPkgCpio(cs cpioStream) ([]Entry, error) {
	var out []Entry
	for {
		hdr, err := cs.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		kind := cpioKind(cpio.FileMode(hdr.Mode))
		out = append(out, Entry{
			Name:    strings.TrimPrefix(hdr.Name, "./"), // pkgbuild writes "./"-rooted names
			Size:    hdr.Size,
			Kind:    kind,
			Mode:    defaultMode(kind, int64(hdr.Mode&0o7777)),
			ModTime: hdr.MTime,
			UID:     int(hdr.UID),
			GID:     int(hdr.GID),
		})
	}
}

// listPkgMember expands one Payload/Scripts member into cpio entries.
func (a *xarArchive) listPkgMember(e xarEntry) ([]Entry, func(), error) {
	r, wrapper, closeFn, err := a.openPkgPayloadStream(e)
	if err != nil {
		return nil, nil, err
	}
	if wrapper != "gzip" && wrapper != "cpio" {
		closeFn()
		return nil, nil, fmt.Errorf("unsupported payload wrapper %q", wrapper)
	}
	cs, err := openCpioStream(r)
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	out, err := listPkgCpio(cs)
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	return out, closeFn, nil
}

// openPkgMember reads one file inside a member's cpio stream: the member is
// decoded streaming and the entry's bytes are served from a bounded reader.
func (a *xarArchive) openPkgMember(e xarEntry, member string) (io.ReadCloser, int64, error) {
	r, wrapper, closeFn, err := a.openPkgPayloadStream(e)
	if err != nil {
		return nil, 0, err
	}
	if wrapper != "gzip" && wrapper != "cpio" {
		closeFn()
		return nil, 0, fmt.Errorf("unsupported payload wrapper %q", wrapper)
	}
	cs, err := openCpioStream(r)
	if err != nil {
		closeFn()
		return nil, 0, err
	}
	for {
		hdr, err := cs.Next()
		if err == io.EOF {
			closeFn()
			return nil, 0, fmt.Errorf("entry %s not found in archive", member)
		}
		if err != nil {
			closeFn()
			return nil, 0, err
		}
		if strings.TrimPrefix(hdr.Name, "./") != member {
			continue
		}
		return &tarEntryReadCloser{Reader: io.LimitReader(cs, hdr.Size), closeFn: closeFn}, hdr.Size, nil
	}
}

// peekReader buffers the head of a stream so the wrapper can be sniffed
// without disturbing the cpio reader that follows.
type peekReader struct {
	r    io.Reader
	buf  []byte
	used int
}

func newPeekReader(r io.Reader) *peekReader { return &peekReader{r: r} }

func (p *peekReader) Peek(n int) ([]byte, error) {
	for len(p.buf) < n {
		chunk := make([]byte, n-len(p.buf))
		m, err := p.r.Read(chunk)
		p.buf = append(p.buf, chunk[:m]...)
		if err != nil {
			return p.buf, err
		}
	}
	return p.buf[:n], nil
}

func (p *peekReader) Read(b []byte) (int, error) {
	if p.used < len(p.buf) {
		n := copy(b, p.buf[p.used:])
		p.used += n
		return n, nil
	}
	return p.r.Read(b)
}

func (a *xarArchive) Open(name string) (io.ReadCloser, int64, error) {
	entries, index, heapBase, err := loadXAR(a.path)
	if err != nil {
		return nil, 0, err
	}
	// A payload-expanded path ("Payload/etc/motd") resolves inside the member's
	// cpio stream; the payload may sit at the top level or inside a nested
	// product directory ("Product.pkg/Payload/...").
	for _, marker := range []string{"/Payload/", "/Scripts/"} {
		if idx := strings.Index(name, marker); idx >= 0 {
			base := name[:idx+len(marker)-1]
			if i, ok := index[base]; ok && entries[i].kind == fsview.KindFile && isPkgPayloadName(base) {
				return a.openPkgMember(entries[i], name[idx+len(marker):])
			}
		}
	}
	for _, marker := range []string{"Payload/", "Scripts/"} {
		if strings.HasPrefix(name, marker) {
			if i, ok := index[marker[:len(marker)-1]]; ok && entries[i].kind == fsview.KindFile && isPkgPayloadName(marker[:len(marker)-1]) {
				return a.openPkgMember(entries[i], name[len(marker):])
			}
		}
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
