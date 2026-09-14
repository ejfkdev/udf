package archive

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/ejfkdev/udf/fsview"
)

// PyInstaller onefile executables append a CArchive overlay to the bootloader
// (PE/ELF/Mach-O). The overlay ends with a cookie carrying the 8-byte magic
// "MEI\014\013\012\013\016", a TOC of zlib-compressed entries, and Python
// modules stored as marshalled code objects. Entries of type 'z'/'Z' are PYZ
// archives: a "PYZ\0" header, the pyc magic, and a marshalled dict TOC of the
// stdlib/application modules they bundle. Listing and extraction reconstruct
// valid .pyc files (header + marshalled code) so decompilers can consume them
// directly, mirroring pyinstxtractor's output layout.

const pyinstMagic = "MEI\x0c\x0b\x0a\x0b\x0e"

// pyinstTailScanLimit bounds the backward cookie search. The cookie normally
// sits within the last few hundred bytes; the slack covers exotic tails.
const pyinstTailScanLimit = 1 << 20

// CArchive TOC entry type codes (PyInstaller archive_viewer conventions).
const (
	pyinstTypeBinary = byte('b') // shared library
	pyinstTypeDep    = byte('d') // dependency declaration, not a file
	pyinstTypePkg    = byte('M') // python package (pyc)
	pyinstTypeMod    = byte('m') // python module (pyc)
	pyinstTypeOpt    = byte('o') // runtime option, not a file
	pyinstTypeSource = byte('s') // entry-point script (marshalled, no pyc header)
	pyinstTypeData   = byte('x') // data file
	pyinstTypePyz    = byte('z') // PYZ archive
	pyinstTypePyzOld = byte('Z') // PYZ archive (legacy spelling)
	pyinstTypeNspkg  = byte('n') // pkgutil-style namespace package marker
	pyinstTypeSplash = byte('l') // splash screen resources
)

type pyinstCookie struct {
	overlayPos int64 // absolute file offset where the CArchive overlay starts
	tocPos     int64 // absolute file offset of the TOC
	tocLen     int64
	pyver      int // 311 for CPython 3.11, 27 for 2.7
}

type pyinstEntry struct {
	name       string
	typeCode   byte
	pos        int64 // absolute file offset of the payload
	csize      int64 // stored (possibly compressed) size
	usize      int64 // uncompressed size
	compressed bool
	bare       bool // python payload stored without a pyc header (needs reconstruction)
}

// pyinstParsed is a fully parsed CArchive: cookie, TOC and the pyc magic
// recovered from an intact module header or a PYZ header when available.
type pyinstParsed struct {
	cookie   pyinstCookie
	entries  []pyinstEntry
	pycMagic []byte // 4 bytes, nil when unknown
}

func (p *pyinstParsed) pyMajor() int { return p.cookie.pyver / 100 }
func (p *pyinstParsed) pyMinor() int { return p.cookie.pyver % 100 }

// pyinstPycHeader builds the 16-byte (3.7+), 12-byte (3.3-3.6) or 8-byte
// (older) pyc header for a bare marshalled code object.
func (p *pyinstParsed) pycHeader() []byte {
	magic := p.pycMagic
	if magic == nil {
		magic = pyinstDefaultPycMagic(p.cookie.pyver)
	}
	var hdr bytes.Buffer
	hdr.Write(magic)
	if p.pyMajor() >= 3 && p.pyMinor() >= 7 {
		hdr.Write(make([]byte, 12)) // PEP 552 flags + (timestamp, size) or hash
	} else {
		hdr.Write(make([]byte, 4)) // timestamp
		if p.pyMajor() >= 3 && p.pyMinor() >= 3 {
			hdr.Write(make([]byte, 4)) // source size (3.3+)
		}
	}
	return hdr.Bytes()
}

// pyinstDefaultPycMagic maps a cookie Python version to that version's
// standard pyc magic, used when the archive carries no intact header. The
// table covers CPython 3.3-3.14; unknown versions fall back to all-zero
// magic which decompilers can still be pointed at manually.
func pyinstDefaultPycMagic(pyver int) []byte {
	switch pyver {
	case 33:
		return []byte{0x9e, 0x0d, 0x0d, 0x0a}
	case 34:
		return []byte{0xee, 0x0c, 0x0d, 0x0a}
	case 35:
		return []byte{0x16, 0x0d, 0x0d, 0x0a}
	case 36:
		return []byte{0x33, 0x0d, 0x0d, 0x0a}
	case 37:
		return []byte{0x42, 0x0d, 0x0d, 0x0a}
	case 38:
		return []byte{0x55, 0x0d, 0x0d, 0x0a}
	case 39:
		return []byte{0x61, 0x0d, 0x0d, 0x0a}
	case 310:
		return []byte{0x6f, 0x0d, 0x0d, 0x0a}
	case 311:
		return []byte{0xa7, 0x0d, 0x0d, 0x0a}
	case 312:
		return []byte{0xcb, 0x0d, 0x0d, 0x0a}
	case 313:
		return []byte{0xf3, 0x0d, 0x0d, 0x0a}
	case 314:
		return []byte{0x2b, 0x0e, 0x0d, 0x0a}
	}
	return []byte{0, 0, 0, 0}
}

// isPyInstaller reports whether path is a PyInstaller CArchive executable.
// It validates by parsing the cookie and the full TOC, so stray magic bytes
// inside unrelated data do not produce false positives.
func isPyInstaller(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = pyinstParse(f)
	return err == nil
}

// pyinstParse locates the cookie, validates it by parsing the whole TOC, and
// recovers the pyc magic when the archive exposes one.
func pyinstParse(f *os.File) (*pyinstParsed, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fileSize := st.Size()
	if fileSize < int64(len(pyinstMagic)) {
		return nil, fmt.Errorf("file is too short to be a pyinstaller archive")
	}

	cookiePos, ok := pyinstFindMagic(f, fileSize)
	if !ok {
		return nil, fmt.Errorf("pyinstaller cookie not found")
	}

	// 2.1+ cookies carry a 64-byte python library name right after the
	// 24-byte base cookie; 2.0 cookies do not.
	cookieSize := int64(24)
	probe := make([]byte, 64)
	n, _ := f.ReadAt(probe, cookiePos+24)
	if n == 64 && bytes.Contains(bytes.ToLower(probe), []byte("python")) {
		cookieSize = 24 + 64
	}

	buf := make([]byte, cookieSize)
	if _, err := f.ReadAt(buf, cookiePos); err != nil {
		return nil, fmt.Errorf("read pyinstaller cookie: %w", err)
	}
	var pkgLen, tocOff, tocLen uint32
	var pyver int32
	pkgLen = binary.BigEndian.Uint32(buf[8:12])
	tocOff = binary.BigEndian.Uint32(buf[12:16])
	tocLen = binary.BigEndian.Uint32(buf[16:20])
	pyver = int32(binary.BigEndian.Uint32(buf[20:24]))

	tailBytes := fileSize - cookiePos - cookieSize
	if tailBytes < 0 {
		return nil, fmt.Errorf("pyinstaller cookie beyond end of file")
	}
	overlayPos := fileSize - int64(pkgLen) - tailBytes
	tocPos := overlayPos + int64(tocOff)
	if overlayPos < 0 || tocPos < 0 || int64(tocLen) <= 0 || tocPos+int64(tocLen) > fileSize {
		return nil, fmt.Errorf("implausible pyinstaller cookie (pkgLen=%d tocOff=%d tocLen=%d)", pkgLen, tocOff, tocLen)
	}

	entries, err := pyinstParseTOC(f, overlayPos, tocPos, int64(tocLen))
	if err != nil {
		return nil, err
	}

	parsed := &pyinstParsed{
		cookie:  pyinstCookie{overlayPos: overlayPos, tocPos: tocPos, tocLen: int64(tocLen), pyver: int(pyver)},
		entries: entries,
	}
	parsed.pycMagic = parsed.findPycMagic(f)
	return parsed, nil
}

// pyinstFindMagic scans the tail of the file backwards for the cookie magic.
func pyinstFindMagic(f *os.File, fileSize int64) (int64, bool) {
	scan := fileSize
	if scan > pyinstTailScanLimit {
		scan = pyinstTailScanLimit
	}
	buf := make([]byte, scan)
	if _, err := f.ReadAt(buf, fileSize-scan); err != nil && err != io.EOF {
		return 0, false
	}
	idx := bytes.LastIndex(buf, []byte(pyinstMagic))
	if idx < 0 {
		return 0, false
	}
	return fileSize - scan + int64(idx), true
}

func pyinstParseTOC(f *os.File, overlayPos, tocPos, tocLen int64) ([]pyinstEntry, error) {
	buf := make([]byte, tocLen)
	if _, err := f.ReadAt(buf, tocPos); err != nil {
		return nil, fmt.Errorf("read pyinstaller TOC: %w", err)
	}

	var entries []pyinstEntry
	off := int64(0)
	for off < tocLen {
		if off+4 > tocLen {
			return nil, fmt.Errorf("truncated pyinstaller TOC entry at offset %d", off)
		}
		entrySize := int64(binary.BigEndian.Uint32(buf[off : off+4]))
		// Fixed part: entryPos, csize, usize (3*4) + flag + type = 14, plus
		// the 4-byte size field itself.
		if entrySize < 18 || off+entrySize > tocLen {
			return nil, fmt.Errorf("implausible pyinstaller TOC entry size %d at offset %d", entrySize, off)
		}
		rec := buf[off+4 : off+entrySize]
		nameLen := entrySize - 18
		e := pyinstEntry{
			pos:        overlayPos + int64(binary.BigEndian.Uint32(rec[0:4])),
			csize:      int64(binary.BigEndian.Uint32(rec[4:8])),
			usize:      int64(binary.BigEndian.Uint32(rec[8:12])),
			compressed: rec[12] == 1,
			typeCode:   rec[13],
			name:       strings.TrimRight(string(rec[14:14+nameLen]), "\x00"),
		}
		e.name = pyinstCleanName(e.name)
		entries = append(entries, e)
		off += entrySize
	}
	return entries, nil
}

// pyinstCleanName sanitizes a TOC entry name: empty or undecodable names get a
// stable placeholder, leading slashes are stripped and ".." is neutralized so
// extraction can never escape the output directory.
func pyinstCleanName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	for strings.HasPrefix(name, "/") {
		name = name[1:]
	}
	name = strings.ReplaceAll(name, "..", "__")
	if name == "" {
		name = "unnamed"
	}
	return name
}

// findPycMagic recovers the 4-byte pyc magic from an intact module header
// (PyInstaller < 5.3) or a PYZ header, preferring PYZ since it is present in
// every onefile build.
func (p *pyinstParsed) findPycMagic(f *os.File) []byte {
	for i := range p.entries {
		e := &p.entries[i]
		if e.typeCode != pyinstTypePyz && e.typeCode != pyinstTypePyzOld {
			continue
		}
		data, err := pyinstReadEntryData(f, e)
		if err != nil || len(data) < 8 || !bytes.HasPrefix(data, []byte("PYZ\x00")) {
			continue
		}
		return data[4:8]
	}
	for i := range p.entries {
		e := &p.entries[i]
		if e.typeCode != pyinstTypeMod && e.typeCode != pyinstTypePkg {
			continue
		}
		var head [8]byte
		n, _ := f.ReadAt(head[:], e.pos)
		if n >= 8 && head[2] == '\r' && head[3] == '\n' {
			return head[0:4]
		}
		if e.compressed {
			data, err := pyinstReadEntryData(f, e)
			if err == nil && len(data) >= 4 && data[2] == '\r' && data[3] == '\n' {
				return data[0:4]
			}
		}
	}
	return nil
}

// pyinstReadEntryData reads and (when flagged) inflates one CArchive entry.
func pyinstReadEntryData(f *os.File, e *pyinstEntry) ([]byte, error) {
	if e.csize < 0 || e.pos < 0 || e.pos+e.csize > fileSizeOf(f) {
		return nil, fmt.Errorf("pyinstaller entry %s out of range", e.name)
	}
	raw := make([]byte, e.csize)
	if _, err := f.ReadAt(raw, e.pos); err != nil && err != io.EOF {
		return nil, fmt.Errorf("read pyinstaller entry %s: %w", e.name, err)
	}
	if !e.compressed {
		return raw, nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("inflate pyinstaller entry %s: %w", e.name, err)
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func fileSizeOf(f *os.File) int64 {
	st, err := f.Stat()
	if err != nil {
		return -1
	}
	return st.Size()
}

// --- PYZ archives ---

// pyzMember is one module inside an inflated PYZ blob.
type pyzMember struct {
	pyzName string // CArchive entry name of the owning PYZ
	off     int64  // offset of the zlib stream inside the PYZ blob
	length  int64  // compressed stream length
	usize   int64  // uncompressed code-object size (0 when inflation failed)
	isPkg   bool
	module  string // dotted module name
}

// parsePyzTOC decodes a PYZ blob's header and marshalled TOC, returning the
// pyc magic recorded in the PYZ and its member list.
func parsePyzTOC(pyzName string, blob []byte) ([]byte, []pyzMember, error) {
	if len(blob) < 12 || !bytes.HasPrefix(blob, []byte("PYZ\x00")) {
		return nil, nil, fmt.Errorf("not a PYZ archive")
	}
	pycMagic := blob[4:8]
	tocPos := int64(int32(binary.BigEndian.Uint32(blob[8:12])))
	if tocPos < 0 || tocPos >= int64(len(blob)) {
		return nil, nil, fmt.Errorf("PYZ TOC position %d out of range", tocPos)
	}
	toc, err := marshalLoad(blob[tocPos:])
	if err != nil {
		return nil, nil, fmt.Errorf("unmarshal PYZ TOC: %w", err)
	}

	var members []pyzMember
	switch t := toc.(type) {
	case map[any]any:
		for key, val := range t {
			m, ok := pyzMemberFromTOCValue(pyzName, key, val)
			if ok {
				members = append(members, m)
			}
		}
	case []any: // PyInstaller < 3.1: list of (name, (ispkg, pos, length))
		for _, item := range t {
			pair, ok := item.([]any)
			if !ok || len(pair) != 2 {
				continue
			}
			m, ok := pyzMemberFromTOCValue(pyzName, pair[0], pair[1])
			if ok {
				members = append(members, m)
			}
		}
	default:
		return nil, nil, fmt.Errorf("unexpected PYZ TOC type %T", toc)
	}
	return pycMagic, members, nil
}

func pyzMemberFromTOCValue(pyzName string, key, val any) (pyzMember, bool) {
	var module string
	switch k := key.(type) {
	case string:
		module = k
	case []byte:
		module = string(k)
	default:
		return pyzMember{}, false
	}
	tuple, ok := val.([]any)
	if !ok || len(tuple) != 3 {
		return pyzMember{}, false
	}
	ispkg, _ := tuple[0].(int64)
	pos, _ := tuple[1].(int64)
	length, _ := tuple[2].(int64)
	if module == "" || pos < 0 || length < 0 {
		return pyzMember{}, false
	}
	return pyzMember{pyzName: pyzName, module: module, off: pos, length: length, isPkg: ispkg == 1}, true
}

// extractName mirrors pyinstxtractor's output naming for a PYZ member.
func (m *pyzMember) extractName() string {
	rel := strings.ReplaceAll(m.module, ".", "/")
	if m.isPkg {
		rel = path.Join(rel, "__init__.pyc")
	} else {
		rel += ".pyc"
	}
	return m.pyzName + "_extracted/" + rel
}

// --- Archive implementation ---

// pyinstArchive exposes a PyInstaller CArchive as a flat archive. Python
// modules and entry-point scripts are presented as reconstructed .pyc files,
// and PYZ archives additionally expose their members under
// "<pyz>_extracted/", matching pyinstxtractor's output layout.
type pyinstArchive struct {
	path string

	parsed   *pyinstParsed
	pyzBlobs map[string][]byte    // PYZ entry name -> inflated blob
	pyzIndex map[string]pyzMember // extraction name -> member
	initErr  error
	inited   bool
}

func (a *pyinstArchive) init() error {
	if a.inited {
		return a.initErr
	}
	a.inited = true
	a.initErr = a.load()
	return a.initErr
}

func (a *pyinstArchive) load() error {
	f, err := os.Open(a.path)
	if err != nil {
		return err
	}
	defer f.Close()

	parsed, err := pyinstParse(f)
	if err != nil {
		return err
	}
	a.parsed = parsed
	a.pyzBlobs = map[string][]byte{}
	a.pyzIndex = map[string]pyzMember{}

	// Mark python payloads stored without a pyc header (PyInstaller 5.3+
	// strips it; entry-point scripts never had one).
	for i := range parsed.entries {
		e := &parsed.entries[i]
		switch e.typeCode {
		case pyinstTypeSource:
			e.bare = true
		case pyinstTypeMod, pyinstTypePkg:
			data, err := pyinstReadEntryData(f, e)
			if err == nil && !(len(data) >= 4 && data[2] == '\r' && data[3] == '\n') {
				e.bare = true
			}
		}
	}

	for i := range parsed.entries {
		e := &parsed.entries[i]
		if e.typeCode != pyinstTypePyz && e.typeCode != pyinstTypePyzOld {
			continue
		}
		blob, err := pyinstReadEntryData(f, e)
		if err != nil {
			continue // a broken PYZ must not hide the rest of the archive
		}
		magic, members, err := parsePyzTOC(e.name, blob)
		if err != nil {
			continue
		}
		if parsed.pycMagic == nil {
			parsed.pycMagic = magic
		}
		for j := range members {
			m := &members[j]
			if m.off >= 0 && m.off+m.length <= int64(len(blob)) {
				if data, err := inflateAll(blob[m.off : m.off+m.length]); err == nil {
					m.usize = int64(len(data))
				}
			}
		}
		a.pyzBlobs[e.name] = blob
		for _, m := range members {
			a.pyzIndex[m.extractName()] = m
		}
	}
	return nil
}

// entryName is the listing/extraction name of a CArchive entry.
func (a *pyinstArchive) entryName(e *pyinstEntry) string {
	switch e.typeCode {
	case pyinstTypeSource, pyinstTypeMod, pyinstTypePkg:
		return e.name + ".pyc"
	}
	return e.name
}

func (a *pyinstArchive) List() ([]Entry, error) {
	if err := a.init(); err != nil {
		return nil, err
	}

	hdrLen := int64(len(a.parsed.pycHeader()))
	var entries []Entry
	for i := range a.parsed.entries {
		e := &a.parsed.entries[i]
		if e.typeCode == pyinstTypeDep || e.typeCode == pyinstTypeOpt {
			continue // runtime metadata, not files
		}
		name := a.entryName(e)
		size := e.usize
		if e.bare {
			size += hdrLen // bare marshalled code gains a pyc header
		}
		entries = append(entries, Entry{
			Name: name,
			Size: size,
			Kind: fsview.KindFile,
			Mode: pyinstEntryMode(e.typeCode),
		})
		if e.typeCode == pyinstTypePyz || e.typeCode == pyinstTypePyzOld {
			for _, m := range a.pyzMembers(e.name) {
				entries = append(entries, Entry{
					Name: m.extractName(),
					Size: m.usize + hdrLen,
					Kind: fsview.KindFile,
					Mode: 0o644,
				})
			}
		}
	}
	return entries, nil
}

// pyzMembers returns the indexed members of one PYZ in stable name order.
func (a *pyinstArchive) pyzMembers(pyzName string) []pyzMember {
	var out []pyzMember
	for _, m := range a.pyzIndex {
		if m.pyzName == pyzName {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].extractName() < out[j].extractName() })
	return out
}

// pyinstEntryMode picks a conventional permission: binaries and the
// bootloader are executable, everything else is 0644.
func pyinstEntryMode(typeCode byte) int64 {
	if typeCode == pyinstTypeBinary || typeCode == pyinstTypeSplash {
		return 0o755
	}
	return 0o644
}

func (a *pyinstArchive) Open(name string) (io.ReadCloser, int64, error) {
	if err := a.init(); err != nil {
		return nil, 0, err
	}

	f, err := os.Open(a.path)
	if err != nil {
		return nil, 0, err
	}

	// PYZ members are served from the cached inflated blob.
	if m, ok := a.pyzIndex[name]; ok {
		blob := a.pyzBlobs[m.pyzName]
		if m.off+m.length > int64(len(blob)) {
			_ = f.Close()
			return nil, 0, fmt.Errorf("PYZ member %s out of range", m.module)
		}
		data, err := inflateAll(blob[m.off : m.off+m.length])
		if err != nil {
			_ = f.Close()
			return nil, 0, fmt.Errorf("inflate PYZ member %s: %w", m.module, err)
		}
		out := append(a.parsed.pycHeader(), data...)
		_ = f.Close()
		return io.NopCloser(bytes.NewReader(out)), int64(len(out)), nil
	}

	for i := range a.parsed.entries {
		e := &a.parsed.entries[i]
		if e.typeCode == pyinstTypeDep || e.typeCode == pyinstTypeOpt {
			continue
		}
		if a.entryName(e) != name {
			continue
		}
		data, err := pyinstReadEntryData(f, e)
		_ = f.Close()
		if err != nil {
			return nil, 0, err
		}
		if e.bare {
			data = append(a.parsed.pycHeader(), data...)
		}
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
	}
	_ = f.Close()
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}

func inflateAll(raw []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

var _ Archive = (*pyinstArchive)(nil)
