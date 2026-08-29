package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
)

func tarWithFile(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func compressRaw(t *testing.T, comp string, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch comp {
	case "gzip":
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	case "zstd":
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	case "xz":
		w, err := xz.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	case "lz4":
		w := lz4.NewWriter(&buf)
		if _, err := w.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestCompressedTarListOpen(t *testing.T) {
	raw := tarWithFile(t)
	cases := []struct {
		comp   string
		detect string
		ext    string
	}{
		{"gzip", "tar.gz", "gz"},
		{"zstd", "tar.zst", "zst"},
		{"xz", "tar.xz", "xz"},
		{"lz4", "tar.lz4", "lz4"},
	}
	for _, c := range cases {
		t.Run(c.comp, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.tar."+c.ext)
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
			if len(entries) != 1 || entries[0].Name != "a.txt" || entries[0].Size != 4 {
				t.Fatalf("entries = %+v", entries)
			}
			rc, size, err := ar.Open("a.txt")
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil || string(b) != "data" || size != 4 {
				t.Fatalf("a.txt = %q (size %d, err %v)", b, size, err)
			}
		})
	}
}
