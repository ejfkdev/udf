package archive

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// arscBuilder writes a resource table byte by byte, from the format itself, so
// the fixture is an independent check of the decoder.
type arscBuilder struct {
	buf bytes.Buffer
}

func (b *arscBuilder) u8(v uint8)   { b.buf.WriteByte(v) }
func (b *arscBuilder) u16(v uint16) { binary.Write(&b.buf, binary.LittleEndian, v) }
func (b *arscBuilder) u32(v uint32) { binary.Write(&b.buf, binary.LittleEndian, v) }

// chunk writes a chunk header plus a body, padded to 4 bytes.
func (b *arscBuilder) chunk(typ, headerSize uint16, body func()) int {
	start := b.buf.Len()
	b.u16(typ)
	b.u16(headerSize)
	b.u32(0) // patched below
	body()
	for b.buf.Len()%4 != 0 {
		b.u8(0)
	}
	end := b.buf.Len()
	binary.LittleEndian.PutUint32(b.buf.Bytes()[start+4:], uint32(end-start))
	return end - start
}

// pool writes a string pool chunk in UTF-8 mode.
func (b *arscBuilder) pool(strings []string) {
	header := 28
	offsets := 4 * len(strings)
	var data bytes.Buffer
	var offs []uint32
	for _, s := range strings {
		offs = append(offs, uint32(data.Len()))
		// UTF-8 entries carry the character count and the byte length, each in
		// the short form for these test strings.
		data.WriteByte(byte(len(s)))
		data.WriteByte(byte(len(s)))
		data.WriteString(s)
		data.WriteByte(0)
	}
	b.chunk(arscStringPool, uint16(header), func() {
		b.u32(uint32(len(strings)))
		b.u32(0)      // style count
		b.u32(1 << 8) // UTF-8 flag
		b.u32(uint32(header + offsets))
		b.u32(0) // styles start
		for _, o := range offs {
			b.u32(o)
		}
		b.buf.Write(data.Bytes())
	})
}

// resValue writes a Res_value.
func (b *arscBuilder) resValue(dataType uint8, data uint32) {
	b.u16(8)
	b.u8(0)
	b.u8(dataType)
	b.u32(data)
}

// buildARSC builds a table with one package (0x7f) holding two types: string
// and attr, across a default and an "en" configuration, plus a style bag.
func buildARSC() []byte {
	var b arscBuilder
	globalStrings := []string{"app_name", "Magisk", "Settings", "primary", "hello"}
	typeStrings := []string{"string", "attr", "style"}
	keyStrings := []string{"app_name", "colorPrimary", "Theme"}

	b.chunk(arscTableType, 12, func() {
		b.u32(1) // package count

		// Global string pool: the values strings point into.
		b.pool(globalStrings)

		// Package 0x7f.
		b.chunk(arscPackageType, 288, func() {
			b.u32(0x7f)
			name := make([]uint16, 128)
			for i, r := range "com.example.app" {
				name[i] = uint16(r)
			}
			for _, u := range name {
				b.u16(u)
			}
			// typeStrings / lastPublicType / keyStrings / lastPublicKey / typeIdOffset
			typeStringsOff := 288 // relative to the package chunk start
			// Both pools follow the header; compute the type pool size first.
			var probe arscBuilder
			probe.pool(typeStrings)
			keyStringsOff := typeStringsOff + probe.buf.Len()
			b.u32(uint32(typeStringsOff))
			b.u32(uint32(len(typeStrings)))
			b.u32(uint32(keyStringsOff))
			b.u32(uint32(len(keyStrings)))
			b.u32(0)

			b.pool(typeStrings)
			b.pool(keyStrings)

			// A type-spec chunk for type 1 (string), as real tables have.
			b.chunk(arscTypeSpec, 16, func() {
				b.u8(1)
				b.u8(0)
				b.u16(0)
				b.u32(2) // entry count
				b.u32(0)
				b.u32(0)
			})

			// Build the entries first so their offsets are known.
			var body bytes.Buffer
			entryOffsets := make([]uint32, 2)
			// entry 0: app_name -> "Magisk"
			entryOffsets[0] = uint32(body.Len())
			body.Write([]byte{8, 0, 0, 0})                      // size, flags
			binary.Write(&body, binary.LittleEndian, uint32(0)) // key index 0
			body.Write([]byte{8, 0, 0, 3})                      // Res_value: size 8, type STRING
			binary.Write(&body, binary.LittleEndian, uint32(1)) // -> "Magisk"
			// entry 1: absent (0xFFFFFFFF)
			entryOffsets[1] = 0xFFFFFFFF

			headerSize := 20 + 64
			offsetsSize := 4 * 2
			entriesStart := headerSize + offsetsSize
			b.chunk(arscTypeType, uint16(headerSize), func() {
				b.u8(1)
				b.u8(0)
				b.u16(0)
				b.u32(2)
				b.u32(uint32(entriesStart))
				config := make([]byte, 64)
				binary.LittleEndian.PutUint32(config[0:], 64)
				b.buf.Write(config)
				for _, o := range entryOffsets {
					b.u32(o)
				}
				b.buf.Write(body.Bytes())
			})

			// Type 2 "attr": one entry with an int value, in a locale config.
			var body2 bytes.Buffer
			body2.Write([]byte{8, 0, 0, 0})
			binary.Write(&body2, binary.LittleEndian, uint32(1)) // key "colorPrimary"
			body2.Write([]byte{8, 0, 0, 0x1d})                   // ARGB8
			binary.Write(&body2, binary.LittleEndian, uint32(0xff2196f3))
			headerSize2 := 20 + 64
			entriesStart2 := headerSize2 + 4
			b.chunk(arscTypeType, uint16(headerSize2), func() {
				b.u8(2)
				b.u8(0)
				b.u16(0)
				b.u32(1)
				b.u32(uint32(entriesStart2))
				config := make([]byte, 64)
				binary.LittleEndian.PutUint32(config[0:], 64)
				config[8] = 'e' // language "en"
				config[9] = 'n'
				b.buf.Write(config)
				b.u32(0)
				b.buf.Write(body2.Bytes())
			})

			// Type 3 "style": a bag with a parent and two items.
			var body3 bytes.Buffer
			body3.Write([]byte{16, 0, 1, 0})                              // size 16, complex
			binary.Write(&body3, binary.LittleEndian, uint32(2))          // key "Theme"
			binary.Write(&body3, binary.LittleEndian, uint32(0x7f020000)) // parent
			binary.Write(&body3, binary.LittleEndian, uint32(2))          // item count
			binary.Write(&body3, binary.LittleEndian, uint32(0x01010098)) // android:colorAccent
			body3.Write([]byte{8, 0, 0, 1})                               // reference
			binary.Write(&body3, binary.LittleEndian, uint32(0x7f020000))
			binary.Write(&body3, binary.LittleEndian, uint32(0x01010036)) // android:windowTitle
			body3.Write([]byte{8, 0, 0, 3})                               // string
			binary.Write(&body3, binary.LittleEndian, uint32(2))          // "Settings"
			headerSize3 := 20 + 64
			entriesStart3 := headerSize3 + 4
			b.chunk(arscTypeType, uint16(headerSize3), func() {
				b.u8(3)
				b.u8(0)
				b.u16(0)
				b.u32(1)
				b.u32(uint32(entriesStart3))
				config := make([]byte, 64)
				binary.LittleEndian.PutUint32(config[0:], 64)
				b.buf.Write(config)
				b.u32(0)
				b.buf.Write(body3.Bytes())
			})
		})
	})
	return b.buf.Bytes()
}

func TestDecodeARSC(t *testing.T) {
	data := buildARSC()
	if !isARSC(data) {
		t.Fatalf("fixture is not recognized as a resource table")
	}
	out, err := DecodeARSC(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Logf("decoded:\n%s", out)

	for _, want := range []string{
		"package 0x7f com.example.app",
		"0x7F010000", "string/app_name", "[default]", `"Magisk"`,
		"0x7F020000", "attr/colorPrimary", "[en]", "#ff2196f3",
		"style/Theme", "bag(2 items)", "parent=@attr/colorPrimary",
		"@0x01010098", `"Settings"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("decoded output is missing %q:\n%s", want, out)
		}
	}
}

// TestARSCZipEntryDecoded checks the APK path: resources.arsc inside a zip is
// served as text, like the binary XML files next to it.
func TestARSCZipEntryDecoded(t *testing.T) {
	path := writeZipWithEntry(t, "resources.arsc", buildARSC())
	format, err := Detect(path)
	if err != nil || format != "zip" {
		t.Fatalf("Detect = %q, %v", format, err)
	}
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rc, size, err := a.Open("resources.arsc")
	if err != nil {
		t.Fatalf("open resources.arsc: %v", err)
	}
	got := make([]byte, size)
	n, _ := rc.Read(got)
	rc.Close()
	text := string(got[:n])
	if !strings.Contains(text, "string/app_name") || !strings.Contains(text, `"Magisk"`) {
		t.Fatalf("zip entry was not decoded:\n%s", text)
	}
}

// writeZipWithEntry builds a zip holding one stored entry.
func writeZipWithEntry(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.apk")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
