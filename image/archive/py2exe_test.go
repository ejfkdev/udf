package archive

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

// The fixture below is a hand-written PE32: DOS header, PE header with one
// .rsrc section, a resource tree whose top-level entry is a named
// PYTHONSCRIPT type (plus an RT_MANIFEST to check plain resources), and an
// import table naming python312.dll. It is written independently of the
// parser, so the parser is tested against bytes rather than against itself.
const (
	peSectVA   = 0x1000
	peRawOff   = 0x200
	peOptSize  = 224
	peHdrEnd   = 0x40 + 4 + 20 + peOptSize + 40
	peNameOff  = 0xc0 // "PYTHONSCRIPT" in the resource section
	relL0      = 0x00
	relL1Named = 0x20
	relL1ID    = 0x38
	relL2Named = 0x50
	relL2ID    = 0x68 // two languages, so this directory holds two entries
	relDataA   = 0x88
	relDataB   = 0x98
	relDataC   = 0xa8
	relScript  = 0xe0
)

type py2exeFixture struct {
	path      string
	zipPath   string
	script    []byte // the marshalled script, as stored
	manifest  []byte // the manifest resource, language 1033
	manifest2 []byte // the same resource in language 1031
	bundle    map[string][]byte
	pythonV   int
}

func putU16(b []byte, v uint16) { binary.LittleEndian.PutUint16(b, v) }
func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }

// buildPy2Exe writes a py2exe-shaped executable; appendZip additionally
// appends a module archive, the way bundle_files<=1 does.
func buildPy2Exe(t *testing.T, appendZip bool) py2exeFixture {
	t.Helper()
	script := []byte("marshalled-code-object-bytes")
	manifest := []byte("<assembly>manifest</assembly>")
	manifest2 := []byte("<assembly>其他语言</assembly>")
	zippath := "library.zip"
	bundle := map[string][]byte{"hello.pyc": []byte("marshalled hello"), "lib/mod.pyc": []byte("marshalled mod")}

	// PYTHONSCRIPT resource: four words, the archive name, the script.
	var scriptRes bytes.Buffer
	var four [16]byte
	putU32(four[0:], py2exeScriptMagic)
	putU32(four[4:], 0) // optimize
	putU32(four[8:], 1) // unbuffered
	putU32(four[12:], uint32(len(script)))
	scriptRes.Write(four[:])
	scriptRes.WriteString(zippath)
	scriptRes.WriteByte(0)
	scriptRes.Write(script)
	scriptRes.WriteByte(0)

	sect := make([]byte, 0x600)
	// Resource tree.
	putU16(sect[relL0+12:], 1) // named entries
	putU16(sect[relL0+14:], 1) // id entries
	putU32(sect[relL0+16+0*8:], 0x80000000|peNameOff)
	putU32(sect[relL0+16+0*8+4:], 0x80000000|relL1Named)
	putU32(sect[relL0+16+1*8:], 24) // RT_MANIFEST
	putU32(sect[relL0+16+1*8+4:], 0x80000000|relL1ID)

	putU16(sect[relL1Named+14:], 1)
	putU32(sect[relL1Named+16:], 1)
	putU32(sect[relL1Named+16+4:], 0x80000000|relL2Named)
	putU16(sect[relL1ID+14:], 1)
	putU32(sect[relL1ID+16:], 1)
	putU32(sect[relL1ID+16+4:], 0x80000000|relL2ID)

	putU16(sect[relL2Named+14:], 1)
	putU32(sect[relL2Named+16:], 1033)
	putU32(sect[relL2Named+16+4:], relDataA)
	putU16(sect[relL2ID+14:], 2) // the manifest in two languages
	putU32(sect[relL2ID+16:], 1033)
	putU32(sect[relL2ID+16+4:], relDataB)
	putU32(sect[relL2ID+24:], 1031)
	putU32(sect[relL2ID+24+4:], relDataC)

	// Name string: length in UTF-16 units, then the units.
	nameUnits := utf16.Encode([]rune("PYTHONSCRIPT"))
	putU16(sect[peNameOff:], uint16(len(nameUnits)))
	for i, u := range nameUnits {
		putU16(sect[peNameOff+2+i*2:], u)
	}

	scriptOff := relScript
	copy(sect[scriptOff:], scriptRes.Bytes())
	manifestOff := scriptOff + scriptRes.Len()
	copy(sect[manifestOff:], manifest)
	manifest2Off := manifestOff + len(manifest)
	copy(sect[manifest2Off:], manifest2)
	putU32(sect[relDataA:], peSectVA+uint32(scriptOff))
	putU32(sect[relDataA+4:], uint32(scriptRes.Len()))
	putU32(sect[relDataB:], peSectVA+uint32(manifestOff))
	putU32(sect[relDataB+4:], uint32(len(manifest)))
	putU32(sect[relDataC:], peSectVA+uint32(manifest2Off))
	putU32(sect[relDataC+4:], uint32(len(manifest2)))

	// Import table: one descriptor naming python312.dll, then a terminator.
	importOff := 0x400
	nameStr := append([]byte("python312.dll"), 0)
	putU32(sect[importOff+12:], peSectVA+uint32(importOff+40))
	copy(sect[importOff+40:], nameStr)

	file := make([]byte, peRawOff+len(sect))
	file[0], file[1] = 'M', 'Z'
	putU32(file[0x3c:], 0x40)
	copy(file[0x40:], "PE\x00\x00")
	putU16(file[0x44:], 0x14c) // i386
	putU16(file[0x46:], 1)     // one section
	putU16(file[0x54:], peOptSize)
	putU16(file[0x56:], 0x010f)
	optOff := 0x58
	putU16(file[optOff:], 0x10b) // PE32
	putU32(file[optOff+32:], 0x1000)
	putU32(file[optOff+36:], 0x200)
	putU32(file[optOff+92:], 16) // NumberOfRvaAndSizes
	putU32(file[optOff+96+1*8:], peSectVA+uint32(importOff))
	putU32(file[optOff+96+1*8+4:], 60)
	putU32(file[optOff+96+2*8:], peSectVA)
	putU32(file[optOff+96+2*8+4:], uint32(len(sect)))

	sectOff := optOff + peOptSize
	copy(file[sectOff:], ".rsrc")
	putU32(file[sectOff+8:], uint32(len(sect)))
	putU32(file[sectOff+12:], peSectVA)
	putU32(file[sectOff+16:], uint32(len(sect)))
	putU32(file[sectOff+20:], peRawOff)
	copy(file[peRawOff:], sect)

	dir := t.TempDir()
	path := filepath.Join(dir, "app.exe")
	if err := os.WriteFile(path, file, 0o755); err != nil {
		t.Fatal(err)
	}
	fx := py2exeFixture{path: path, script: script, manifest: manifest, manifest2: manifest2, bundle: bundle, pythonV: 312}
	if appendZip {
		zp := filepath.Join(dir, "library.zip")
		f, err := os.Create(zp)
		if err != nil {
			t.Fatal(err)
		}
		zw := zip.NewWriter(f)
		for _, name := range []string{"hello.pyc", "lib/mod.pyc"} {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(bundle[name]); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		zb, err := os.ReadFile(zp)
		if err != nil {
			t.Fatal(err)
		}
		withZip := filepath.Join(dir, "app-onefile.exe")
		if err := os.WriteFile(withZip, append(append([]byte{}, file...), zb...), 0o755); err != nil {
			t.Fatal(err)
		}
		fx.zipPath = withZip
	}
	return fx
}

func TestPy2ExeDetectAndList(t *testing.T) {
	fx := buildPy2Exe(t, true)

	format, err := Detect(fx.zipPath)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if format != "py2exe" {
		// A one-file exe also looks like a zip-sfx; py2exe must win.
		t.Fatalf("Detect = %q, want py2exe", format)
	}
	if format, err := Detect(fx.path); err != nil || format != "py2exe" {
		t.Fatalf("Detect (no bundle) = %q, %v", format, err)
	}

	a, err := Open(fx.zipPath)
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
	// The manifest ships in two languages, so both entries carry the language.
	if _, ok := byName["resource/RT_MANIFEST/1.1033"]; !ok {
		t.Fatalf("manifest entry missing from %v", names(entries))
	}
	if _, ok := byName["resource/RT_MANIFEST/1.1031"]; !ok {
		t.Fatalf("second-language manifest missing from %v", names(entries))
	}
	if _, ok := byName["bundle/hello.pyc"]; !ok {
		t.Fatalf("bundle entry missing from %v", names(entries))
	}
	scriptEntry, ok := byName["resource/PYTHONSCRIPT/1.pyc"]
	if !ok {
		t.Fatalf("decoded script missing from %v", names(entries))
	}
	if want := a.(*py2exeArchive).pycHeader(); scriptEntry.Size != int64(len(want)+len(fx.script)) {
		t.Fatalf("script size = %d, want %d", scriptEntry.Size, len(want)+len(fx.script))
	}

	// The decoded script is a pyc whose header names CPython 3.12.
	got := readAll(t, a, "resource/PYTHONSCRIPT/1.pyc")
	want := append(pyinstDefaultPycMagic(fx.pythonV), make([]byte, 12)...)
	want = append(want, fx.script...)
	if !bytes.Equal(got, want) {
		t.Fatalf("script = %x, want %x", got, want)
	}

	if got := readAll(t, a, "resource/RT_MANIFEST/1.1033"); !bytes.Equal(got, fx.manifest) {
		t.Fatalf("manifest = %q", got)
	}
	if got := readAll(t, a, "resource/RT_MANIFEST/1.1031"); !bytes.Equal(got, fx.manifest2) {
		t.Fatalf("second-language manifest = %q", got)
	}
	for name, data := range fx.bundle {
		if got := readAll(t, a, "bundle/"+name); !bytes.Equal(got, data) {
			t.Fatalf("bundle/%s = %q, want %q", name, got, data)
		}
	}
}

func TestPy2ExeNoBundleAndNoScript(t *testing.T) {
	fx := buildPy2Exe(t, false)
	a, err := Open(fx.path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entries, err := a.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, e := range entries {
		if len(e.Name) > len(py2exeBundlePfx) && e.Name[:len(py2exeBundlePfx)] == py2exeBundlePfx {
			t.Fatalf("unexpected bundle entry %q", e.Name)
		}
	}
	if _, _, err := a.Open("bundle/hello.pyc"); err == nil {
		t.Fatal("bundle entry opened without an appended archive")
	}

	// A PE without PYTHONSCRIPT is not py2exe, even with resources.
	plain := writePlainPEWithResources(t)
	if format, err := Detect(plain); err != nil || format == "py2exe" {
		t.Fatalf("Detect(plain) = %q, %v", format, err)
	}
	if isPy2Exe(plain) {
		t.Fatal("isPy2Exe said yes without a PYTHONSCRIPT resource")
	}
}

// writePlainPEWithResources builds the same PE but with the manifest resource
// only.
func writePlainPEWithResources(t *testing.T) string {
	t.Helper()
	fx := buildPy2Exe(t, false)
	file, err := os.ReadFile(fx.path)
	if err != nil {
		t.Fatal(err)
	}
	// Point the level-0 named entry at an id-only subtree by renaming the type
	// string: overwrite "PYTHONSCRIPT" with a different same-length name.
	copy(file[peRawOff+peNameOff+2:], []byte{'X', 0, 'Y', 0, 'Z', 0, '1', 0, '2', 0, '3', 0, '4', 0, '5', 0, '6', 0, '7', 0, '8', 0})
	path := filepath.Join(t.TempDir(), "plain.exe")
	if err := os.WriteFile(path, file, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func names(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}
