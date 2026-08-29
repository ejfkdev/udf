package archive

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cavaliergopher/cpio"
)

// cpioFixture builds an in-memory newc cpio holding a regular file and a
// directory, exercising the compression path without an external tool.
func cpioFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := cpio.NewWriter(&buf)
	if err := w.WriteHeader(&cpio.Header{Name: "hello.txt", Mode: 0o100644, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteHeader(&cpio.Header{Name: "sub/", Mode: 0o040755}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCompressedCPIOListOpen(t *testing.T) {
	raw := cpioFixture(t)
	cases := []struct {
		comp   string
		detect string
		ext    string
	}{
		{"gzip", "cpio.gz", "gz"},
		{"zstd", "cpio.zst", "zst"},
		{"xz", "cpio.xz", "xz"},
		{"lz4", "cpio.lz4", "lz4"},
	}
	for _, c := range cases {
		t.Run(c.comp, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.cpio."+c.ext)
			if err := os.WriteFile(path, compressRaw(t, c.comp, raw), 0o644); err != nil {
				t.Fatal(err)
			}
			if f, err := Detect(path); err != nil || f != c.detect {
				t.Fatalf("Detect = %q, %v; want %q", f, err, c.detect)
			}
			ar, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := ar.List()
			if err != nil {
				t.Fatal(err)
			}
			names := map[string]bool{}
			for _, e := range entries {
				names[e.Name] = true
			}
			if !names["hello.txt"] || !names["sub/"] {
				t.Fatalf("listing = %v", names)
			}
			rc, size, err := ar.Open("hello.txt")
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil || string(b) != "hello" || size != 5 {
				t.Fatalf("hello.txt = %q (size %d, err %v)", b, size, err)
			}
		})
	}
}
