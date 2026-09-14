package archive

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/ejfkdev/udf/fsview"
	"github.com/klauspost/compress/zstd"
)

type nuitkaFixtureFile struct {
	name    string
	content []byte
	exec    bool
}

// buildNuitkaOnefile assembles a synthetic appended-payload onefile binary,
// mirroring Nuitka's OnefileCompressor writer: host stub + "KA" + indicator
// + entry stream (optionally zstd) + [windows: alignment padding] + uint64
// payload size trailer.
func buildNuitkaOnefile(t *testing.T, compressed, windows bool, files []nuitkaFixtureFile) string {
	t.Helper()

	var stream bytes.Buffer
	for _, f := range files {
		var nameBytes []byte
		if windows {
			units := utf16.Encode([]rune(f.name + "\x00"))
			nameBytes = make([]byte, len(units)*2)
			for i, u := range units {
				binary.LittleEndian.PutUint16(nameBytes[i*2:], u)
			}
		} else {
			nameBytes = append([]byte(f.name), 0)
		}
		stream.Write(nameBytes)
		var sizeBuf [8]byte
		binary.LittleEndian.PutUint64(sizeBuf[:], uint64(len(f.content)))
		stream.Write(sizeBuf[:])
		if !windows {
			flags := byte(0)
			if f.exec {
				flags = 1
			}
			stream.WriteByte(flags)
		}
		stream.Write(f.content)
	}
	if windows {
		stream.Write([]byte{0, 0}) // UTF-16 NUL terminator
	} else {
		stream.WriteByte(0)
	}

	payload := stream.Bytes()
	indicator := byte('X')
	if compressed {
		indicator = 'Y'
		enc, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		payload = enc.EncodeAll(stream.Bytes(), nil)
	}

	var buf bytes.Buffer
	host := make([]byte, 512)
	copy(host, []byte{0x7f, 'E', 'L', 'F'})
	buf.Write(host)
	startPos := int64(buf.Len())
	buf.Write([]byte{'K', 'A', indicator})
	buf.Write(payload)
	if windows {
		if pad := buf.Len() % 8; pad != 0 {
			buf.Write(make([]byte, 8-pad))
		}
	}
	endPos := int64(buf.Len())
	var trailer [8]byte
	binary.LittleEndian.PutUint64(trailer[:], uint64(endPos-startPos))
	buf.Write(trailer[:])

	path := filepath.Join(t.TempDir(), "nuitka-app")
	if err := os.WriteFile(path, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

var nuitkaTestFiles = []nuitkaFixtureFile{
	{name: "app.bin", content: bytes.Repeat([]byte("binary!"), 100), exec: true},
	{name: "etc/config.json", content: []byte(`{"k":1}`)},
}

func TestNuitkaOnefileStoredPosix(t *testing.T) {
	path := buildNuitkaOnefile(t, false, false, nuitkaTestFiles)
	checkNuitkaFixture(t, path)
}

func TestNuitkaOnefileZstdPosix(t *testing.T) {
	path := buildNuitkaOnefile(t, true, false, nuitkaTestFiles)
	checkNuitkaFixture(t, path)
}

func TestNuitkaOnefileWindowsUTF16(t *testing.T) {
	path := buildNuitkaOnefile(t, true, true, nuitkaTestFiles)

	format, err := Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if format != "nuitka" {
		t.Fatalf("Detect = %q, want nuitka", format)
	}
	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := readAll(t, a, "app.bin"); !bytes.Equal(got, nuitkaTestFiles[0].content) {
		t.Fatalf("app.bin mismatch: %d bytes", len(got))
	}
	if got := readAll(t, a, "etc/config.json"); string(got) != `{"k":1}` {
		t.Fatalf("config.json = %q", got)
	}
}

func checkNuitkaFixture(t *testing.T, path string) {
	t.Helper()

	format, err := Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if format != "nuitka" {
		t.Fatalf("Detect = %q, want nuitka", format)
	}

	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entries, err := a.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List returned %d entries: %v", len(entries), entries)
	}
	byName := map[string]int64{}
	for _, e := range entries {
		byName[e.Name] = e.Mode
	}
	if byName["app.bin"] != 0o755 {
		t.Fatalf("app.bin mode = %o, want 755", byName["app.bin"])
	}
	if byName["etc/config.json"] != 0o644 {
		t.Fatalf("config.json mode = %o, want 644", byName["etc/config.json"])
	}

	if got := readAll(t, a, "app.bin"); !bytes.Equal(got, nuitkaTestFiles[0].content) {
		t.Fatalf("app.bin mismatch: %d bytes", len(got))
	}
	if got := readAll(t, a, "etc/config.json"); string(got) != `{"k":1}` {
		t.Fatalf("config.json = %q", got)
	}
}

func TestNuitkaNegative(t *testing.T) {
	// ELF binary whose trailing 8 bytes accidentally look large.
	path := filepath.Join(t.TempDir(), "plain-elf")
	host := make([]byte, 4096)
	copy(host, []byte{0x7f, 'E', 'L', 'F'})
	binary.LittleEndian.PutUint64(host[len(host)-8:], 100)
	if err := os.WriteFile(path, host, 0o755); err != nil {
		t.Fatal(err)
	}
	format, err := Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if format != "" {
		t.Fatalf("Detect = %q, want empty", format)
	}
}

// buildNuitkaV2Stream writes the Nuitka 2.x POSIX layout: name, flags,
// [symlink target name], size, data.
func buildNuitkaV2Stream(files []nuitkaFixtureFile, symlinks [][2]string) []byte {
	var stream bytes.Buffer
	write := func(f nuitkaFixtureFile) {
		stream.Write(append([]byte(f.name), 0))
		flags := byte(0)
		if f.exec {
			flags = 1
		}
		stream.WriteByte(flags)
		var sizeBuf [8]byte
		binary.LittleEndian.PutUint64(sizeBuf[:], uint64(len(f.content)))
		stream.Write(sizeBuf[:])
		stream.Write(f.content)
	}
	for _, f := range files {
		write(f)
	}
	for _, s := range symlinks {
		stream.Write(append([]byte(s[0]), 0))
		stream.WriteByte(2) // symlink flag
		stream.Write(append([]byte(s[1]), 0))
	}
	stream.WriteByte(0) // terminator
	return stream.Bytes()
}

func TestNuitkaV2LayoutWithSymlink(t *testing.T) {
	stream := buildNuitkaV2Stream(nuitkaTestFiles, [][2]string{{"bin/python", "python3.11"}})

	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := enc.EncodeAll(stream, nil)

	var buf bytes.Buffer
	host := make([]byte, 512)
	copy(host, []byte{0x7f, 'E', 'L', 'F'})
	buf.Write(host)
	startPos := int64(buf.Len())
	// Embedded-style: no trailer; detection scans for "KAY"+zstd magic.
	buf.Write([]byte{'K', 'A', 'Y'})
	buf.Write(compressed)
	buf.Write(bytes.Repeat([]byte{0}, 256)) // trailing binary data
	_ = startPos

	path := filepath.Join(t.TempDir(), "nuitka-v2")
	if err := os.WriteFile(path, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}

	format, err := Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if format != "nuitka" {
		t.Fatalf("Detect = %q, want nuitka", format)
	}

	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entries, err := a.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var foundLink bool
	for _, e := range entries {
		if e.Name == "bin/python" {
			foundLink = true
			if e.Kind != fsview.KindSymlink || e.Linkname != "python3.11" {
				t.Fatalf("bin/python = kind %v target %q", e.Kind, e.Linkname)
			}
		}
	}
	if !foundLink {
		t.Fatalf("symlink entry missing from %v", entries)
	}
	if got := readAll(t, a, "app.bin"); !bytes.Equal(got, nuitkaTestFiles[0].content) {
		t.Fatalf("app.bin mismatch: %d bytes", len(got))
	}
}
