package archive

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// dotnetString writes a .NET BinaryReader-style length-prefixed string.
func dotnetString(s string) []byte {
	out := []byte{byte(len(s))} // fixtures only use short strings
	return append(out, s...)
}

type dotnetFixtureFile struct {
	path       string
	content    []byte
	fileType   byte
	compressed bool
}

// buildDotnetBundle assembles a synthetic single-file bundle: MZ host stub,
// 8-byte manifest address + 32-byte signature, file payloads, then the
// manifest header, matching the layout written by the official bundler.
func buildDotnetBundle(t *testing.T, major uint32, files []dotnetFixtureFile) string {
	t.Helper()

	var buf bytes.Buffer
	host := make([]byte, 512)
	copy(host, []byte("MZ"))
	buf.Write(host)
	manifestAddrPos := int64(buf.Len())
	buf.Write(make([]byte, 8)) // patched later
	buf.Write(dotnetBundleSignature)

	offsets := make([]int64, len(files))
	sizes := make([]int64, len(files))
	stored := make([][]byte, len(files))
	for i, f := range files {
		offsets[i] = int64(buf.Len())
		data := f.content
		if f.compressed && major >= 6 {
			var cbuf bytes.Buffer
			fw, err := flate.NewWriter(&cbuf, flate.DefaultCompression)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fw.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := fw.Close(); err != nil {
				t.Fatal(err)
			}
			data = cbuf.Bytes()
		}
		stored[i] = data
		sizes[i] = int64(len(f.content))
		buf.Write(data)
	}

	manifestAddr := int64(buf.Len())
	var m bytes.Buffer
	put32 := func(v uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		m.Write(b[:])
	}
	put64 := func(v int64) {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(v))
		m.Write(b[:])
	}
	put32(major)
	put32(0) // minor
	put32(uint32(len(files)))
	m.Write(dotnetString("bundle-id-test"))
	if major >= 2 {
		put64(0) // deps.json offset
		put64(0) // deps.json size
		put64(0) // runtimeconfig.json offset
		put64(0) // runtimeconfig.json size
		put64(0) // flags
	}
	for i, f := range files {
		put64(offsets[i])
		put64(sizes[i])
		if major >= 6 {
			if f.compressed {
				put64(int64(len(stored[i])))
			} else {
				put64(0)
			}
		}
		m.WriteByte(f.fileType)
		m.Write(dotnetString(f.path))
	}
	buf.Write(m.Bytes())

	// Patch the manifest address in front of the signature.
	out := buf.Bytes()
	binary.LittleEndian.PutUint64(out[manifestAddrPos:], uint64(manifestAddr))

	path := filepath.Join(t.TempDir(), "dotnet-app.exe")
	if err := os.WriteFile(path, out, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDotnetBundleV1(t *testing.T) {
	path := buildDotnetBundle(t, 1, []dotnetFixtureFile{
		{path: "App.dll", content: []byte("assembly-bytes"), fileType: dotnetFileTypeAssembly},
		{path: "libnative.so", content: []byte("native-bytes"), fileType: dotnetFileTypeNative},
	})

	format, err := Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if format != "dotnet-bundle" {
		t.Fatalf("Detect = %q, want dotnet-bundle", format)
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
	if got := readAll(t, a, "App.dll"); string(got) != "assembly-bytes" {
		t.Fatalf("App.dll = %q", got)
	}
	if got := readAll(t, a, "libnative.so"); string(got) != "native-bytes" {
		t.Fatalf("libnative.so = %q", got)
	}
}

func TestDotnetBundleV6Compressed(t *testing.T) {
	content := bytes.Repeat([]byte("compressed-payload!"), 200)
	path := buildDotnetBundle(t, 6, []dotnetFixtureFile{
		{path: "App.dll", content: []byte("assembly"), fileType: dotnetFileTypeAssembly, compressed: true},
		{path: "big.bin", content: content, fileType: dotnetFileTypeNative, compressed: true},
		{path: "appsettings.json", content: []byte("{}"), fileType: dotnetFileTypeUnknown},
	})

	format, err := Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if format != "dotnet-bundle" {
		t.Fatalf("Detect = %q, want dotnet-bundle", format)
	}

	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := readAll(t, a, "big.bin"); !bytes.Equal(got, content) {
		t.Fatalf("big.bin mismatch: %d bytes", len(got))
	}
	if got := readAll(t, a, "appsettings.json"); string(got) != "{}" {
		t.Fatalf("appsettings.json = %q", got)
	}
}

func TestDotnetBundleNegative(t *testing.T) {
	// A plain MZ binary without signature must not be detected.
	path := filepath.Join(t.TempDir(), "plain.exe")
	host := make([]byte, 4096)
	copy(host, []byte("MZ"))
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
