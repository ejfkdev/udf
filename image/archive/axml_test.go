package archive

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// --- minimal AXML writer for fixtures ---

type axmlWriter struct {
	strings []string
	index   map[string]uint32
	chunks  []byte
}

func newAXMLWriter() *axmlWriter {
	return &axmlWriter{index: map[string]uint32{}}
}

func (w *axmlWriter) str(s string) uint32 {
	if idx, ok := w.index[s]; ok {
		return idx
	}
	idx := uint32(len(w.strings))
	w.strings = append(w.strings, s)
	w.index[s] = idx
	return idx
}

func axmlChunk(typ uint16, body []byte) []byte {
	out := make([]byte, 8+len(body))
	binary.LittleEndian.PutUint16(out[0:], typ)
	binary.LittleEndian.PutUint16(out[2:], 8) // patched by callers needing more
	binary.LittleEndian.PutUint32(out[4:], uint32(8+len(body)))
	copy(out[8:], body)
	return out
}

func (w *axmlWriter) nodeChunk(typ uint16, ext []byte) {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint32(body[0:], 1)          // lineNumber
	binary.LittleEndian.PutUint32(body[4:], 0xffffffff) // comment
	body = append(body, ext...)
	c := axmlChunk(typ, body)
	binary.LittleEndian.PutUint16(c[2:], 16) // node header size
	w.chunks = append(w.chunks, c...)
}

func (w *axmlWriter) nsStart(prefix, uri string) {
	ext := make([]byte, 8)
	binary.LittleEndian.PutUint32(ext[0:], w.str(prefix))
	binary.LittleEndian.PutUint32(ext[4:], w.str(uri))
	w.nodeChunk(axmlNsStartType, ext)
}

type axmlAttr struct {
	ns       string // "" for no namespace
	name     string
	rawValue string // "" when the typed value carries the data
	dt       byte
	data     uint32
}

func (w *axmlWriter) elemStart(name string, attrs []axmlAttr) {
	ext := make([]byte, 20)
	binary.LittleEndian.PutUint32(ext[0:], 0xffffffff) // ns
	binary.LittleEndian.PutUint32(ext[4:], w.str(name))
	binary.LittleEndian.PutUint16(ext[8:], 20)  // attributeStart
	binary.LittleEndian.PutUint16(ext[10:], 20) // attributeSize
	binary.LittleEndian.PutUint16(ext[12:], uint16(len(attrs)))
	for _, a := range attrs {
		rec := make([]byte, 20)
		nsIdx := uint32(0xffffffff)
		if a.ns != "" {
			nsIdx = w.str(a.ns)
		}
		rawIdx := uint32(0xffffffff)
		if a.rawValue != "" {
			rawIdx = w.str(a.rawValue)
		}
		binary.LittleEndian.PutUint32(rec[0:], nsIdx)
		binary.LittleEndian.PutUint32(rec[4:], w.str(a.name))
		binary.LittleEndian.PutUint32(rec[8:], rawIdx)
		binary.LittleEndian.PutUint16(rec[12:], 8) // typed value size
		rec[14] = 0
		rec[15] = a.dt
		binary.LittleEndian.PutUint32(rec[16:], a.data)
		ext = append(ext, rec...)
	}
	w.nodeChunk(axmlElemStartType, ext)
}

func (w *axmlWriter) elemEnd(name string) {
	ext := make([]byte, 8)
	binary.LittleEndian.PutUint32(ext[0:], 0xffffffff)
	binary.LittleEndian.PutUint32(ext[4:], w.str(name))
	w.nodeChunk(axmlElemEndType, ext)
}

// finish builds the string pool (UTF-8 or UTF-16) and the file chunk.
func (w *axmlWriter) finish(t *testing.T, utf8 bool) []byte {
	t.Helper()

	var data bytes.Buffer
	offsets := make([]uint32, len(w.strings))
	for i, s := range w.strings {
		offsets[i] = uint32(data.Len())
		if utf8 {
			data.WriteByte(byte(len([]rune(s))))
			data.WriteByte(byte(len(s)))
			data.WriteString(s)
			data.WriteByte(0)
		} else {
			units := utf16Encode(s)
			var l [2]byte
			binary.LittleEndian.PutUint16(l[:], uint16(len(units)))
			data.Write(l[:])
			for _, u := range units {
				binary.LittleEndian.PutUint16(l[:], u)
				data.Write(l[:])
			}
			data.Write([]byte{0, 0})
		}
		for data.Len()%4 != 0 {
			data.WriteByte(0)
		}
	}

	stringsStart := uint32(28 + 4*len(w.strings))
	pool := make([]byte, 28)
	binary.LittleEndian.PutUint16(pool[0:], axmlStringPoolType)
	binary.LittleEndian.PutUint16(pool[2:], 28)
	flags := uint32(0)
	if utf8 {
		flags = 1 << 8
	}
	binary.LittleEndian.PutUint32(pool[8:], uint32(len(w.strings)))
	binary.LittleEndian.PutUint32(pool[12:], 0) // styleCount
	binary.LittleEndian.PutUint32(pool[16:], flags)
	binary.LittleEndian.PutUint32(pool[20:], stringsStart)
	binary.LittleEndian.PutUint32(pool[24:], 0) // stylesStart
	for _, o := range offsets {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], o)
		pool = append(pool, b[:]...)
	}
	pool = append(pool, data.Bytes()...)
	binary.LittleEndian.PutUint32(pool[4:], uint32(len(pool)))

	file := make([]byte, 8)
	binary.LittleEndian.PutUint16(file[0:], axmlResXMLType)
	binary.LittleEndian.PutUint16(file[2:], 8)
	file = append(file, pool...)
	file = append(file, w.chunks...)
	binary.LittleEndian.PutUint32(file[4:], uint32(len(file)))
	return file
}

func utf16Encode(s string) []uint16 {
	var out []uint16
	for _, r := range s {
		if r > 0xffff {
			r -= 0x10000
			out = append(out, uint16(0xd800+r>>10), uint16(0xdc00+r&0x3ff))
			continue
		}
		out = append(out, uint16(r))
	}
	return out
}

// --- tests ---

const androidNS = "http://schemas.android.com/apk/res/android"

func buildTestAXML(t *testing.T, utf8 bool) []byte {
	t.Helper()
	w := newAXMLWriter()
	w.nsStart("android", androidNS)
	w.elemStart("manifest", []axmlAttr{
		{name: "package", rawValue: "com.example.app", dt: axmlTypeString},
		{ns: androidNS, name: "versionCode", dt: axmlTypeIntDec, data: 221},
		{ns: androidNS, name: "debuggable", dt: axmlTypeIntBool, data: 1},
	})
	w.elemStart("application", []axmlAttr{
		{ns: androidNS, name: "theme", dt: axmlTypeReference, data: 0x01030006},
	})
	w.elemStart("activity", []axmlAttr{
		{ns: androidNS, name: "name", rawValue: ".MainActivity", dt: axmlTypeString},
	})
	w.elemEnd("activity")
	w.elemEnd("application")
	w.elemEnd("manifest")
	return w.finish(t, utf8)
}

func TestDecodeAXMLUTF8(t *testing.T) {
	data := buildTestAXML(t, true)
	if !isAXML(data) {
		t.Fatal("fixture must be detected as AXML")
	}
	out, err := DecodeAXML(data)
	if err != nil {
		t.Fatalf("DecodeAXML: %v", err)
	}
	for _, want := range []string{
		`<manifest xmlns:android="http://schemas.android.com/apk/res/android"`,
		`package="com.example.app"`,
		`android:versionCode="221"`,
		`android:debuggable="true"`,
		`android:theme="@android:0x1030006"`,
		`<activity android:name=".MainActivity" />`,
		`</application></manifest>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("decoded XML missing %q:\n%s", want, out)
		}
	}
}

func TestDecodeAXMLUTF16(t *testing.T) {
	data := buildTestAXML(t, false)
	out, err := DecodeAXML(data)
	if err != nil {
		t.Fatalf("DecodeAXML: %v", err)
	}
	if !strings.Contains(out, `package="com.example.app"`) {
		t.Fatalf("decoded XML missing package attr:\n%s", out)
	}
	if !strings.Contains(out, `android:versionCode="221"`) {
		t.Fatalf("decoded XML missing versionCode:\n%s", out)
	}
}

func TestDecodeAXMLRejectsNonAXML(t *testing.T) {
	if _, err := DecodeAXML([]byte("<?xml version=\"1.0\"?>")); err == nil {
		t.Fatal("text XML must not decode as AXML")
	}
	if isAXML([]byte("PK\x03\x04rest")) {
		t.Fatal("zip magic must not look like AXML")
	}
}
