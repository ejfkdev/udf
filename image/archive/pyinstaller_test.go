package archive

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// --- minimal marshal writer for building PYZ TOC fixtures ---

func mShortStr(s string) []byte {
	return append([]byte{'z', byte(len(s))}, s...)
}

func mInt(v int32) []byte {
	b := make([]byte, 5)
	b[0] = 'i'
	binary.LittleEndian.PutUint32(b[1:], uint32(v))
	return b
}

func mSmallTuple(items ...[]byte) []byte {
	out := []byte{')', byte(len(items))}
	for _, it := range items {
		out = append(out, it...)
	}
	return out
}

func mList(items ...[]byte) []byte {
	out := make([]byte, 5)
	out[0] = '['
	binary.LittleEndian.PutUint32(out[1:], uint32(len(items)))
	for _, it := range items {
		out = append(out, it...)
	}
	return out
}

func zlibCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildPyinstFixture assembles a synthetic PyInstaller 2.1+ cookie CArchive:
// a bootloader stub, an 's' entry-point script, an 'x' data file, a 'b'
// binary, skipped 'd'/'o' metadata entries and an uncompressed PYZ holding
// two modules.
func buildPyinstFixture(t *testing.T) string {
	t.Helper()

	pyzMagic := []byte{0xa7, 0x0d, 0x0d, 0x0a} // CPython 3.11
	mod0 := []byte("marshalled-code-object-0")
	mod1 := []byte("marshalled-code-object-1")
	z0 := zlibCompress(t, mod0)
	z1 := zlibCompress(t, mod1)

	var pyz bytes.Buffer
	pyz.Write([]byte("PYZ\x00"))
	pyz.Write(pyzMagic)
	pos0 := int64(12)
	pos1 := pos0 + int64(len(z0))
	tocPos := pos1 + int64(len(z1))
	var tocPosBuf [4]byte
	binary.BigEndian.PutUint32(tocPosBuf[:], uint32(tocPos))
	pyz.Write(tocPosBuf[:])
	pyz.Write(z0)
	pyz.Write(z1)
	pyz.Write(mList(
		mSmallTuple(mShortStr("mymod"), mSmallTuple(mInt(0), mInt(int32(pos0)), mInt(int32(len(z0))))),
		mSmallTuple(mShortStr("mypkg"), mSmallTuple(mInt(1), mInt(int32(pos1)), mInt(int32(len(z1))))),
	))
	pyzBlob := pyz.Bytes()

	// CArchive payload entries, in TOC order.
	type payload struct {
		name     string
		typeCode byte
		data     []byte
		compress bool
		skip     bool // dependency/runtime-option metadata
	}
	payloads := []payload{
		{name: "main", typeCode: 's', data: []byte("bare-entrypoint-code")},
		{name: "data.bin", typeCode: 'x', data: []byte("raw-data")},
		{name: "libfoo.so", typeCode: 'b', data: []byte("elf-bytes"), compress: true},
		{name: "pyi-dep", typeCode: 'd', data: []byte("dep"), skip: true},
		{name: "pyi-opt", typeCode: 'o', data: nil, skip: true},
		{name: "../evil", typeCode: 'x', data: []byte("escape")},
		{name: "PYZ.pyz", typeCode: 'z', data: pyzBlob},
	}

	var blob bytes.Buffer // data section
	var toc bytes.Buffer
	for _, p := range payloads {
		stored := p.data
		if p.compress {
			stored = zlibCompress(t, p.data)
		}
		entryPos := uint32(blob.Len())
		blob.Write(stored)

		flag := byte(0)
		if p.compress {
			flag = 1
		}
		nameBytes := append([]byte(p.name), 0) // NUL-terminated
		entrySize := 4 + 14 + len(nameBytes)
		var hdr [18]byte
		binary.BigEndian.PutUint32(hdr[0:4], uint32(entrySize))
		binary.BigEndian.PutUint32(hdr[4:8], entryPos)
		binary.BigEndian.PutUint32(hdr[8:12], uint32(len(stored)))
		binary.BigEndian.PutUint32(hdr[12:16], uint32(len(p.data)))
		hdr[16] = flag
		hdr[17] = p.typeCode
		toc.Write(hdr[:])
		toc.Write(nameBytes)
	}

	// Overlay = data + TOC + cookie.
	tocOff := uint32(blob.Len())
	tocLen := uint32(toc.Len())
	cookieSize := 24 + 64
	pkgLen := uint32(blob.Len()) + tocLen + uint32(cookieSize)

	var overlay bytes.Buffer
	overlay.Write(blob.Bytes())
	overlay.Write(toc.Bytes())
	overlay.Write([]byte(pyinstMagic))
	var ck [16]byte
	binary.BigEndian.PutUint32(ck[0:4], pkgLen)
	binary.BigEndian.PutUint32(ck[4:8], tocOff)
	binary.BigEndian.PutUint32(ck[8:12], tocLen)
	binary.BigEndian.PutUint32(ck[12:16], 311) // pyver: CPython 3.11
	overlay.Write(ck[:])
	pylib := make([]byte, 64)
	copy(pylib, "libpython3.11.so")
	overlay.Write(pylib)

	// Bootloader stub in front of the overlay.
	var exe bytes.Buffer
	exe.Write(bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 64))
	exe.Write(overlay.Bytes())

	dir := t.TempDir()
	path := filepath.Join(dir, "app")
	if err := os.WriteFile(path, exe.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPyinstDetect(t *testing.T) {
	path := buildPyinstFixture(t)
	format, err := Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if format != "pyinstaller" {
		t.Fatalf("Detect = %q, want pyinstaller", format)
	}
}

func TestPyinstListAndOpen(t *testing.T) {
	path := buildPyinstFixture(t)
	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	entries, err := a.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byName := map[string]Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	for _, want := range []string{
		"main.pyc", "data.bin", "libfoo.so", "__/evil", "PYZ.pyz",
		"PYZ.pyz_extracted/mymod.pyc", "PYZ.pyz_extracted/mypkg/__init__.pyc",
	} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("missing %q in listing %v", want, keysOf(byName))
		}
	}
	for _, unwanted := range []string{"pyi-dep", "pyi-opt"} {
		if _, ok := byName[unwanted]; ok {
			t.Fatalf("metadata entry %q should not be listed", unwanted)
		}
	}

	hdr := []byte{0xa7, 0x0d, 0x0d, 0x0a, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

	// 's' entry: bare marshalled code rebuilt into a .pyc.
	data := readAll(t, a, "main.pyc")
	if !bytes.Equal(data, append(hdr, []byte("bare-entrypoint-code")...)) {
		t.Fatalf("main.pyc = %q", data)
	}

	// Plain data and compressed binary entries.
	if got := readAll(t, a, "data.bin"); string(got) != "raw-data" {
		t.Fatalf("data.bin = %q", got)
	}
	if got := readAll(t, a, "libfoo.so"); string(got) != "elf-bytes" {
		t.Fatalf("libfoo.so = %q", got)
	}

	// PYZ members come back as header + inflated code object.
	if got := readAll(t, a, "PYZ.pyz_extracted/mymod.pyc"); !bytes.Equal(got, append(hdr, []byte("marshalled-code-object-0")...)) {
		t.Fatalf("mymod.pyc = %q", got)
	}
	if got := readAll(t, a, "PYZ.pyz_extracted/mypkg/__init__.pyc"); !bytes.Equal(got, append(hdr, []byte("marshalled-code-object-1")...)) {
		t.Fatalf("mypkg/__init__.pyc = %q", got)
	}

	// The raw PYZ blob is still available.
	if got := readAll(t, a, "PYZ.pyz"); !bytes.HasPrefix(got, []byte("PYZ\x00")) {
		t.Fatalf("PYZ.pyz does not start with the PYZ magic")
	}
}

func readAll(t *testing.T, a Archive, name string) []byte {
	t.Helper()
	rc, _, err := a.Open(name)
	if err != nil {
		t.Fatalf("Open %s: %v", name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

func keysOf(m map[string]Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestMarshalReaderBasics(t *testing.T) {
	// dict form used by PyInstaller >= 3.1 PYZ TOCs
	toc := marshalDictFixture()
	v, err := marshalLoad(toc)
	if err != nil {
		t.Fatalf("marshalLoad dict: %v", err)
	}
	m, ok := v.(map[any]any)
	if !ok {
		t.Fatalf("expected map, got %T", v)
	}
	tuple, ok := m["mod"].([]any)
	if !ok || len(tuple) != 3 {
		t.Fatalf("expected 3-tuple for key mod, got %v", m["mod"])
	}
	if tuple[1].(int64) != 12 || tuple[2].(int64) != 34 {
		t.Fatalf("unexpected tuple values: %v", tuple)
	}
}

func marshalDictFixture() []byte {
	// {'mod': (0, 12, 34)} terminated by TYPE_NULL
	out := []byte{'{'}
	out = append(out, mShortStr("mod")...)
	out = append(out, mSmallTuple(mInt(0), mInt(12), mInt(34))...)
	out = append(out, '0')
	return out
}
