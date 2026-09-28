package archive

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// A py2exe executable is a Windows PE with two things udf can read: a
// PYTHONSCRIPT resource holding the bootstrap script (a four-word header, the
// name of the module archive, then the script as a marshalled code object) and,
// when everything is bundled into one file, the module archive itself appended
// to the exe as a plain ZIP.
//
// udf lists the resources it finds (decoding PYTHONSCRIPT into a .pyc) and the
// entries of the appended archive under a "bundle/" prefix. The Python version
// the header needs comes from the exe itself — the pythonXY.dll it imports, or
// the resource of the same name that bundling puts there.
//
// Reference: py2exe's own runtime.py (which writes the resource and appends the
// archive) and unpy2exe (the extractor whose layout this follows).
const (
	py2exeScriptMagic = 0x78563412
	py2exeBundlePfx   = "bundle/"
	py2exeMaxScript   = 64 << 20

	// pe directories and resource types used here.
	peResourceDirIndex = 2
	peImportDirIndex   = 1
)

type peResource struct {
	typeName string
	typeID   uint32
	name     string
	nameID   uint32
	langID   uint32
	fileOff  int64
	size     int64
}

type py2exeArchive struct {
	path      string
	resources []peResource

	pythonVersion int  // e.g. 312, when the exe says which pythonXY.dll it uses
	hasBundle     bool // the exe carries an appended module archive
	bundle        *zipArchive
}

// isPy2Exe reports whether path is a py2exe executable: a PE whose resources
// include PYTHONSCRIPT. That resource is what py2exe writes to carry its
// bootstrap, so this is a positive identification rather than a guess.
func isPy2Exe(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if !hasHostPrefix(f, nativeHostPrefixes) {
		return false
	}
	resources, err := peResourceList(f)
	if err != nil {
		return false
	}
	for _, r := range resources {
		if strings.EqualFold(peTypeName(r), "PYTHONSCRIPT") {
			return true
		}
	}
	return false
}

// openPy2Exe parses a py2exe executable.
func openPy2Exe(path string) (*py2exeArchive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	resources, err := peResourceList(f)
	if err != nil {
		return nil, err
	}
	a := &py2exeArchive{path: path, resources: resources, pythonVersion: pePythonVersion(f, resources)}
	// A bundled archive is a plain ZIP appended to the exe; the zip reader
	// finds it from the end of the file, prefix and all.
	zr := &zipArchive{path: path}
	if entries, err := zr.List(); err == nil && len(entries) > 0 {
		a.hasBundle = true
		a.bundle = zr
	}
	return a, nil
}

// pePythonVersion finds the Python version the exe was built for: the
// pythonXY.dll in its import table, or the resource bundling leaves behind.
// Zero when the exe does not say.
var pePythonName = regexp.MustCompile(`(?i)python(\d)(\d+)\.dll`)

func pePythonVersion(f *os.File, resources []peResource) int {
	for _, r := range resources {
		if m := pePythonName.FindStringSubmatch(r.typeName + " " + r.name); m != nil {
			if v, err := strconv.Atoi(m[1] + m[2]); err == nil {
				return v
			}
		}
	}
	if imports, err := peImportedDLLs(f); err == nil {
		for _, name := range imports {
			if m := pePythonName.FindStringSubmatch(name); m != nil {
				if v, err := strconv.Atoi(m[1] + m[2]); err == nil {
					return v
				}
			}
		}
	}
	return 0
}

// peImage is the little that is needed of a PE to map addresses to file
// offsets: the sections.
type peImage struct {
	sections []peSection
}

type peSection struct {
	vaSize  uint32
	vaStart uint32
	rawOff  uint32
	rawSize uint32
}

func (img *peImage) offset(rva uint32) (int64, bool) {
	for _, s := range img.sections {
		if rva >= s.vaStart && rva < s.vaStart+maxU32(s.vaSize, s.rawSize) {
			return int64(s.rawOff + (rva - s.vaStart)), true
		}
	}
	return 0, false
}

func maxU32(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}

// peDirs are the data directory addresses parsePE reports: the resource
// directory (RVA and size) and the import directory.
type peDirs struct {
	resource     uint32
	resourceSize uint32
	importRVA    uint32
}

// parsePE reads the headers and section table.
func parsePE(f *os.File) (*peImage, peDirs, error) {
	var dos [64]byte
	if _, err := f.ReadAt(dos[:], 0); err != nil {
		return nil, peDirs{}, err
	}
	if dos[0] != 'M' || dos[1] != 'Z' {
		return nil, peDirs{}, fmt.Errorf("not a PE file")
	}
	e_lfanew := int64(binary.LittleEndian.Uint32(dos[0x3c:0x40]))
	var sig [24]byte
	if _, err := f.ReadAt(sig[:], e_lfanew); err != nil {
		return nil, peDirs{}, fmt.Errorf("truncated PE header")
	}
	if string(sig[:4]) != "PE\x00\x00" {
		return nil, peDirs{}, fmt.Errorf("not a PE file")
	}
	numSections := int(binary.LittleEndian.Uint16(sig[6:8]))
	optSize := int(binary.LittleEndian.Uint16(sig[20:22]))
	optOff := e_lfanew + 24

	// The optional header's magic says whether the directories hold 32- or
	// 64-bit addresses; the resource and import directories are what matter.
	var opt [8]byte
	if _, err := f.ReadAt(opt[:], optOff); err != nil {
		return nil, peDirs{}, fmt.Errorf("truncated PE optional header")
	}
	is64 := binary.LittleEndian.Uint16(opt[:2]) == 0x20b
	dirOff := optOff + 96
	if is64 {
		dirOff = optOff + 112
	}
	dirEntry := func(i int) (rva, size uint32) {
		var b [8]byte
		if _, err := f.ReadAt(b[:], dirOff+int64(i*8)); err != nil {
			return 0, 0
		}
		return binary.LittleEndian.Uint32(b[:4]), binary.LittleEndian.Uint32(b[4:])
	}
	dirs := peDirs{}
	dirs.resource, dirs.resourceSize = dirEntry(peResourceDirIndex)
	dirs.importRVA, _ = dirEntry(peImportDirIndex)

	img := &peImage{}
	sectOff := optOff + int64(optSize)
	for i := 0; i < numSections; i++ {
		var sh [40]byte
		if _, err := f.ReadAt(sh[:], sectOff+int64(i*40)); err != nil {
			return nil, peDirs{}, fmt.Errorf("truncated PE section table")
		}
		img.sections = append(img.sections, peSection{
			vaSize:  binary.LittleEndian.Uint32(sh[8:12]),
			vaStart: binary.LittleEndian.Uint32(sh[12:16]),
			rawSize: binary.LittleEndian.Uint32(sh[16:20]),
			rawOff:  binary.LittleEndian.Uint32(sh[20:24]),
		})
	}
	if dirs.resource == 0 || dirs.resourceSize == 0 {
		return img, dirs, fmt.Errorf("no resources in this executable")
	}
	return img, dirs, nil
}

// peResourceList walks the resource directory: type, then name, then language,
// each level either a numeric id or a name string.
func peResourceList(f *os.File) ([]peResource, error) {
	img, dirs, err := parsePE(f)
	if err != nil {
		return nil, err
	}
	base, ok := img.offset(dirs.resource)
	if !ok {
		return nil, fmt.Errorf("resource directory is not in any section")
	}

	var out []peResource
	var walk func(off int64, depth int, res *peResource)
	walk = func(off int64, depth int, res *peResource) {
		if depth > 3 {
			return
		}
		var hdr [16]byte
		if _, err := f.ReadAt(hdr[:], off); err != nil {
			return
		}
		named := binary.LittleEndian.Uint16(hdr[12:14])
		ids := binary.LittleEndian.Uint16(hdr[14:16])
		for i := 0; i < int(named)+int(ids); i++ {
			var ent [8]byte
			if _, err := f.ReadAt(ent[:], off+16+int64(i*8)); err != nil {
				return
			}
			nameField := binary.LittleEndian.Uint32(ent[:4])
			child := binary.LittleEndian.Uint32(ent[4:])
			r := *res
			label := ""
			if nameField&0x80000000 != 0 {
				// A name: a length-prefixed UTF-16 string in the directory.
				nameOff := base + int64(nameField&0x7fffffff)
				var ln [2]byte
				if _, err := f.ReadAt(ln[:], nameOff); err == nil {
					n := int(binary.LittleEndian.Uint16(ln[:]))
					buf := make([]byte, n*2)
					if _, err := f.ReadAt(buf, nameOff+2); err == nil {
						label = decodeUTF16(utf16Units(buf))
					}
				}
			} else {
				label = "#" + strconv.FormatUint(uint64(nameField), 10)
			}
			switch depth {
			case 0:
				r.typeName, r.typeID = label, nameField
			case 1:
				r.name, r.nameID = label, nameField
			default:
				r.langID = nameField
				// The leaf points at the data entry.
				dataOff := base + int64(child)
				var d [16]byte
				if _, err := f.ReadAt(d[:], dataOff); err != nil {
					return
				}
				dataRVA := binary.LittleEndian.Uint32(d[:4])
				size := binary.LittleEndian.Uint32(d[4:8])
				fileOff, ok := img.offset(dataRVA)
				if !ok {
					continue
				}
				r.fileOff, r.size = fileOff, int64(size)
				out = append(out, r)
				continue
			}
			walk(base+int64(child&0x7fffffff), depth+1, &r)
		}
	}
	walk(base, 0, &peResource{})
	if len(out) == 0 {
		return nil, fmt.Errorf("no readable resources in this executable")
	}
	return out, nil
}

func utf16Units(b []byte) []uint16 {
	out := make([]uint16, len(b)/2)
	for i := range out {
		out[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return out
}

// peImportedDLLs lists the DLLs a PE imports.
func peImportedDLLs(f *os.File) ([]string, error) {
	img, dirs, err := parsePE(f)
	if err != nil && dirs.importRVA == 0 {
		return nil, err
	}
	if dirs.importRVA == 0 {
		return nil, fmt.Errorf("no import directory")
	}
	base, ok := img.offset(dirs.importRVA)
	if !ok {
		return nil, fmt.Errorf("import directory is not in any section")
	}
	var out []string
	for i := 0; i < 256; i++ {
		var d [20]byte
		if _, err := f.ReadAt(d[:], base+int64(i*20)); err != nil {
			break
		}
		rva := binary.LittleEndian.Uint32(d[12:16])
		if rva == 0 {
			break
		}
		off, ok := img.offset(rva)
		if !ok {
			break
		}
		name, err := readCStringAt(f, off, 256)
		if err != nil {
			break
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no imports")
	}
	return out, nil
}

func readCStringAt(f *os.File, off int64, limit int) (string, error) {
	buf := make([]byte, limit)
	n, err := f.ReadAt(buf, off)
	if err != nil && n == 0 {
		return "", err
	}
	if i := bytes.IndexByte(buf[:n], 0); i >= 0 {
		return string(buf[:i]), nil
	}
	return string(buf[:n]), nil
}

// scriptResource returns the PYTHONSCRIPT resource, when the exe has one.
func (a *py2exeArchive) scriptResource() (peResource, bool) {
	for _, r := range a.resources {
		if strings.EqualFold(r.typeName, "PYTHONSCRIPT") || strings.EqualFold(peTypeName(r), "PYTHONSCRIPT") {
			return r, true
		}
	}
	return peResource{}, false
}

// peTypeName renders a resource type id as its conventional name for the
// numeric types (icons, manifests, version info).
func peTypeName(r peResource) string {
	if strings.HasPrefix(r.typeName, "#") {
		switch r.typeName {
		case "#3":
			return "RT_ICON"
		case "#14":
			return "RT_GROUP_ICON"
		case "#16":
			return "RT_VERSION"
		case "#24":
			return "RT_MANIFEST"
		}
	}
	return r.typeName
}

// resourceEntries names every resource. The name is the resource path to a
// single byte range, so the same name always opens the same bytes.
type resourceEntry struct {
	name string
	res  peResource
}

// resourceEntries names every resource. A type/name pair that appears in more
// than one language gets the language appended, so real executables — which
// commonly ship icons and version info per language — list every one of them
// under a distinct name.
func (a *py2exeArchive) resourceEntries() []resourceEntry {
	counts := map[string]int{}
	base := func(r peResource) string {
		typ := strings.TrimPrefix(peTypeName(r), "#")
		name := strings.TrimPrefix(r.name, "#")
		if name == "" {
			name = strconv.FormatUint(uint64(r.nameID), 10)
		}
		return "resource/" + typ + "/" + name
	}
	for _, r := range a.resources {
		counts[base(r)]++
	}
	out := make([]resourceEntry, 0, len(a.resources))
	for _, r := range a.resources {
		name := base(r)
		if counts[name] > 1 {
			name += "." + strconv.FormatUint(uint64(r.langID), 10)
		}
		out = append(out, resourceEntry{name: name, res: r})
	}
	return out
}

// resourceEntryName is the name one resource lists under.
func resourceEntryName(r peResource) string {
	typ := strings.TrimPrefix(peTypeName(r), "#")
	name := strings.TrimPrefix(r.name, "#")
	if name == "" {
		name = strconv.FormatUint(uint64(r.nameID), 10)
	}
	return "resource/" + typ + "/" + name
}

// scriptPyc turns the PYTHONSCRIPT resource into a pyc: its four-word header,
// the archive name, and the script as a marshalled code object. The pyc header
// that makes the file loadable carries the build's Python magic when the exe
// names its pythonXY.dll, and zeros otherwise.
func (a *py2exeArchive) scriptPyc() ([]byte, bool) {
	r, ok := a.scriptResource()
	if !ok || r.size <= 0 || r.size > py2exeMaxScript {
		return nil, false
	}
	f, err := os.Open(a.path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	data := make([]byte, r.size)
	if _, err := f.ReadAt(data, r.fileOff); err != nil {
		return nil, false
	}
	if len(data) < 16 {
		return nil, false
	}
	if binary.LittleEndian.Uint32(data[:4]) != py2exeScriptMagic {
		return nil, false
	}
	// The header's fourth word is the exact length of the marshalled script,
	// so the trailing NUL py2exe appends never has to be guessed at.
	scriptLen := int64(binary.LittleEndian.Uint32(data[12:16]))
	rest := data[16:]
	i := bytes.IndexByte(rest, 0) // end of the archive name
	if i < 0 {
		return nil, false
	}
	script := rest[i+1:]
	if scriptLen <= 0 || scriptLen > int64(len(script)) {
		scriptLen = int64(len(script))
	}
	script = script[:scriptLen]
	if len(script) == 0 {
		return nil, false
	}
	return append(a.pycHeader(), script...), true
}

// pycHeader builds the header for a bare marshalled code object.
func (a *py2exeArchive) pycHeader() []byte {
	magic := pyinstDefaultPycMagic(a.pythonVersion)
	var hdr bytes.Buffer
	hdr.Write(magic)
	if a.pythonVersion >= 37 || a.pythonVersion == 0 {
		hdr.Write(make([]byte, 12))
	} else {
		hdr.Write(make([]byte, 4))
		if a.pythonVersion >= 33 {
			hdr.Write(make([]byte, 4))
		}
	}
	return hdr.Bytes()
}

func (a *py2exeArchive) List() ([]Entry, error) {
	var entries []Entry
	for _, re := range a.resourceEntries() {
		r := re.res
		if strings.EqualFold(peTypeName(r), "PYTHONSCRIPT") {
			// Listed decoded, so the bootstrap script is readable as a pyc.
			if pyc, ok := a.scriptPyc(); ok {
				entries = append(entries, Entry{
					Name: re.name + ".pyc",
					Size: int64(len(pyc)),
					Kind: kindOfFile("x.pyc"),
					Mode: defaultMode(kindOfFile("x.pyc"), 0o644),
				})
			}
			continue
		}
		kind := kindOfFile(r.name)
		entries = append(entries, Entry{
			Name: re.name,
			Size: r.size,
			Kind: kind,
			Mode: defaultMode(kind, 0o644),
		})
	}
	if a.hasBundle {
		bundleEntries, err := a.bundle.List()
		if err == nil {
			for _, e := range bundleEntries {
				e.Name = py2exeBundlePfx + e.Name
				entries = append(entries, e)
			}
		}
	}
	return entries, nil
}

func (a *py2exeArchive) Open(name string) (io.ReadCloser, int64, error) {
	if strings.HasPrefix(name, py2exeBundlePfx) {
		if !a.hasBundle {
			return nil, 0, fmt.Errorf("entry %s not found in archive", name)
		}
		rc, size, err := a.bundle.Open(strings.TrimPrefix(name, py2exeBundlePfx))
		if err != nil {
			return nil, 0, fmt.Errorf("entry %s not found in archive", name)
		}
		return rc, size, nil
	}
	if strings.HasSuffix(name, ".pyc") && strings.Contains(name, "PYTHONSCRIPT") {
		if pyc, ok := a.scriptPyc(); ok {
			return io.NopCloser(bytes.NewReader(pyc)), int64(len(pyc)), nil
		}
		return nil, 0, fmt.Errorf("entry %s cannot be decoded", name)
	}
	for _, re := range a.resourceEntries() {
		if re.name != name {
			continue
		}
		r := re.res
		f, err := os.Open(a.path)
		if err != nil {
			return nil, 0, err
		}
		return &sectionReadCloser{SectionReader: io.NewSectionReader(f, r.fileOff, r.size), closeFn: f.Close}, r.size, nil
	}
	return nil, 0, fmt.Errorf("entry %s not found in archive", name)
}

var _ Archive = (*py2exeArchive)(nil)
