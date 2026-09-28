package archive

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cavaliergopher/cpio"

	"github.com/ejfkdev/udf/fsview"
)

// An Android boot image (boot.img / recovery.img) is the kernel, the ramdisk
// and, in newer layouts, extra blobs, packed with page-aligned headers so a
// bootloader can map each part. udf exposes every component and, when the
// ramdisk is a compressed cpio archive (the usual case), the files inside it
// under a "ramdisk/" prefix — which is what people actually want to look at.
//
// Layouts:
//
//	v0-v2 (page-aligned, header in the first page): magic, kernel, ramdisk,
//	      second, then (v1) recovery_dtbo and (v2) dtb
//	v3-v4 (4096-aligned, header padded): kernel, ramdisk, then (v4) a boot
//	      signature
//
// The magic alone is weak ("ANDROID!" occurs in random binaries), so the header
// is validated: a plausible page size, a known header version, and component
// ranges that fit inside the file.
const (
	bootMagic         = "ANDROID!"
	bootMagicSize     = 8
	bootNameSize      = 16
	bootArgsSize      = 512
	bootExtraArgsSize = 1024

	// bootMaxRamdisk bounds how much of a ramdisk is unpacked to list its
	// files; larger ones stay a single component entry.
	bootMaxRamdisk = 256 << 20
)

// bootComponent is one part of a boot image: a name and its byte range.
type bootComponent struct {
	name string
	off  int64
	size int64
}

type bootImgArchive struct {
	path  string
	parts []bootComponent

	ramdiskOnce bool
	ramdisk     []ramdiskEntry // nil when the ramdisk has no browsable tree
	ramdiskData []byte
}

// ramdiskEntry is one file inside the ramdisk's cpio, with its data kept in the
// archive's decompressed buffer.
type ramdiskEntry struct {
	hdr  cpio.Header
	off  int
	size int
}

// openBootImg validates an Android boot image and records where each component
// lives.
func openBootImg(path string) (*bootImgArchive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, 4096)
	n, err := io.ReadFull(f, hdr)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	hdr = hdr[:n]
	if len(hdr) < 44 || string(hdr[:bootMagicSize]) != bootMagic {
		return nil, fmt.Errorf("not an Android boot image")
	}
	le := binary.LittleEndian
	headerVersion := le.Uint32(hdr[40:44])
	if headerVersion > 4 {
		return nil, fmt.Errorf("unsupported boot image header version %d", headerVersion)
	}

	a := &bootImgArchive{path: path}
	var page int64
	if headerVersion >= 3 {
		// v3/v4 fixed the page size at 4096 and padded the header to it.
		page = 4096
	} else {
		page = int64(le.Uint32(hdr[36:40]))
		if page < 512 || page > 65536 || page%512 != 0 {
			return nil, fmt.Errorf("implausible boot image page size %d", page)
		}
		headerVersion = le.Uint32(hdr[40:44]) // v0 images carry 0 here
	}

	kernelSize := int64(le.Uint32(hdr[8:12]))
	ramdiskSize := int64(le.Uint32(hdr[16:20]))
	secondSize := int64(0)
	var recoveryDtboSize, dtbSize int64
	if headerVersion <= 2 {
		secondSize = int64(le.Uint32(hdr[24:28]))
		if headerVersion >= 1 && len(hdr) >= 1648 {
			recoveryDtboSize = int64(le.Uint32(hdr[1632:1636]))
		}
		if headerVersion >= 2 && len(hdr) >= 1656 {
			dtbSize = int64(le.Uint32(hdr[1648:1652]))
		}
	}

	// Walk the components in the order the format lays them out, checking each
	// range fits: that is what makes a false positive on the magic harmless.
	pos := page
	add := func(name string, size int64) error {
		if size <= 0 {
			return nil
		}
		end := pos + size
		if end > st.Size() {
			return fmt.Errorf("boot image %s extends past the end of the file", name)
		}
		a.parts = append(a.parts, bootComponent{name: name, off: pos, size: size})
		pos = align(page, end)
		return nil
	}
	for _, part := range []struct {
		name string
		size int64
	}{
		{"kernel", kernelSize},
		{"ramdisk", ramdiskSize},
		{"second", secondSize},
		{"recovery_dtbo", recoveryDtboSize},
		{"dtb", dtbSize},
	} {
		if err := add(part.name, part.size); err != nil {
			return nil, err
		}
	}
	if len(a.parts) == 0 {
		return nil, fmt.Errorf("boot image holds no components")
	}
	return a, nil
}

func align(page, v int64) int64 {
	if page <= 1 {
		return v
	}
	return (v + page - 1) / page * page
}

// bootCompression recognises the compression of a ramdisk, if any.
func bootCompression(b []byte) string {
	switch {
	case len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b:
		return "gzip"
	case len(b) >= 3 && string(b[:3]) == "BZh":
		return "bzip2"
	case len(b) >= 6 && string(b[:6]) == "\xfd7zXZ\x00":
		return "xz"
	case len(b) >= 4 && string(b[:4]) == "\x28\xb5\x2f\xfd":
		return "zstd"
	case len(b) >= 4 && string(b[:4]) == "\x04\x22\x4d\x18":
		return "lz4"
	case len(b) >= 3 && string(b[:3]) == "\x5d\x00\x00":
		return "lzma"
	}
	return ""
}

// loadRamdisk decompresses the ramdisk once and walks its cpio, keeping the
// entries and their data in memory (bounded by bootMaxRamdisk).
func (a *bootImgArchive) loadRamdisk() {
	if a.ramdiskOnce {
		return
	}
	a.ramdiskOnce = true

	var comp *bootComponent
	for i := range a.parts {
		if a.parts[i].name == "ramdisk" {
			comp = &a.parts[i]
			break
		}
	}
	if comp == nil || comp.size > bootMaxRamdisk {
		return
	}
	f, err := os.Open(a.path)
	if err != nil {
		return
	}
	defer f.Close()
	data := make([]byte, comp.size)
	if _, err := f.ReadAt(data, comp.off); err != nil {
		return
	}

	var rd io.Reader = bytes.NewReader(data)
	if c := bootCompression(data); c != "" {
		dr, closeDec, err := openCompressed(bytes.NewReader(data), c)
		if err != nil {
			return
		}
		defer closeDec()
		rd = dr
	}
	raw, err := readAllLimited(rd, bootMaxRamdisk)
	if err != nil {
		return
	}
	entries, ok := parseNewc(raw)
	if !ok {
		return
	}
	a.ramdiskData = raw
	a.ramdisk = entries
}

// parseNewc walks a "newc"/"crc" cpio archive held in memory, returning every
// entry with the position of its data. It is written directly rather than
// through a reader because the archive is already in memory and the offsets of
// the data are what the archive needs to serve single files cheaply.
func parseNewc(b []byte) ([]ramdiskEntry, bool) {
	const (
		newcMagic = "070701"
		crcMagic  = "070702"
	)
	pos := 0
	var out []ramdiskEntry
	for {
		if pos+110 > len(b) {
			return nil, false
		}
		magic := string(b[pos : pos+6])
		if magic != newcMagic && magic != crcMagic {
			return nil, false
		}
		num := func(i int) int64 {
			var v int64
			for _, c := range b[pos+6+8*i : pos+6+8*i+8] {
				v = v<<4 | int64(hexValue(c))
			}
			return v
		}
		mode := num(1)
		uid := num(2)
		gid := num(3)
		size := num(6)
		mtime := num(5)
		namesize := num(11)
		if namesize <= 0 || size < 0 {
			return nil, false
		}
		nameStart := pos + 110
		nameEnd := nameStart + int(namesize)
		if nameEnd > len(b) {
			return nil, false
		}
		name := string(b[nameStart : nameEnd-1]) // the stored name is NUL terminated
		if name == "TRAILER!!!" {
			return out, true
		}
		dataStart := alignTo4(nameEnd)
		dataEnd := dataStart + int(size)
		if dataEnd > len(b) {
			return nil, false
		}
		entry := ramdiskEntry{
			hdr: cpio.Header{
				Name:    strings.TrimPrefix(name, "./"),
				Mode:    cpio.FileMode(mode),
				Size:    size,
				ModTime: unixTime(mtime),
				Uid:     int(uid),
				Guid:    int(gid),
			},
			off:  dataStart,
			size: int(size),
		}
		// A symlink stores its target as the entry's data, which the reader
		// turns into the link name (the header has no field for it).
		if mode&0o170000 == 0o120000 {
			entry.hdr.Linkname = string(b[dataStart:dataEnd])
			entry.hdr.Size = 0
			entry.size = 0
		}
		out = append(out, entry)
		pos = alignTo4(dataEnd)
	}
}

func alignTo4(v int) int { return (v + 3) &^ 3 }

// unixTime turns a cpio timestamp into a time, tolerating the odd values old
// ramdisks carry.
func unixTime(sec int64) time.Time {
	if sec <= 0 || sec > 1<<40 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

func hexValue(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}

// readAllLimited reads a stream up to limit bytes, reporting an error when the
// stream is larger (the caller then leaves the component undecoded).
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("stream exceeds %d bytes", limit)
	}
	return buf.Bytes(), nil
}

// ramdiskPrefix marks entries that live inside the ramdisk.
const ramdiskPrefix = "ramdisk/"

func (a *bootImgArchive) List() ([]Entry, error) {
	a.loadRamdisk()

	entries := make([]Entry, 0, len(a.parts)+len(a.ramdisk))
	// The components, then the ramdisk's own files, so a boot image can be
	// browsed like a rootfs as well as unpacked piece by piece.
	for _, part := range a.parts {
		entries = append(entries, Entry{
			Name: part.name,
			Size: part.size,
			Kind: fsview.KindFile,
			Mode: defaultMode(fsview.KindFile, 0o644),
		})
	}
	if a.ramdisk == nil {
		return entries, nil
	}
	for _, e := range a.ramdisk {
		kind := cpioKind(e.hdr.Mode)
		entries = append(entries, Entry{
			Name:     ramdiskPrefix + strings.TrimPrefix(e.hdr.Name, "/"),
			Size:     int64(e.size),
			Kind:     kind,
			Mode:     defaultMode(kind, int64(uint32(e.hdr.Mode)&0o7777)),
			ModTime:  e.hdr.ModTime,
			UID:      e.hdr.Uid,
			GID:      e.hdr.Guid,
			Linkname: e.hdr.Linkname,
		})
	}
	return entries, nil
}

func (a *bootImgArchive) Open(name string) (io.ReadCloser, int64, error) {
	if strings.HasPrefix(name, ramdiskPrefix) {
		a.loadRamdisk()
		if a.ramdisk == nil {
			return nil, 0, fmt.Errorf("ramdisk of %s cannot be listed", a.path)
		}
		want := strings.TrimPrefix(name, ramdiskPrefix)
		for _, e := range a.ramdisk {
			if strings.TrimPrefix(e.hdr.Name, "/") != want {
				continue
			}
			if kind := cpioKind(e.hdr.Mode); kind != fsview.KindFile {
				return nil, 0, fmt.Errorf("entry %s is not a regular file", name)
			}
			data := a.ramdiskData[e.off : e.off+e.size]
			return io.NopCloser(bytes.NewReader(data)), int64(e.size), nil
		}
		return nil, 0, fmt.Errorf("entry %s not found in archive", name)
	}

	for _, part := range a.parts {
		if part.name != name {
			continue
		}
		f, err := os.Open(a.path)
		if err != nil {
			return nil, 0, err
		}
		return &sectionReadCloser{SectionReader: io.NewSectionReader(f, part.off, part.size), closeFn: f.Close}, part.size, nil
	}
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}

// sectionReadCloser serves a byte range of a file and closes it when done.
type sectionReadCloser struct {
	*io.SectionReader
	closeFn func() error
}

func (s *sectionReadCloser) Close() error { return s.closeFn() }

var _ Archive = (*bootImgArchive)(nil)
