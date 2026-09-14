package archive

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// buildSFXZip concatenates an MZ-prefixed host stub with a real ZIP, the way
// launch4j-style self-extracting executables are made.
func buildSFXZip(t *testing.T, hostPrefix []byte, withZip bool) string {
	t.Helper()

	var buf bytes.Buffer
	host := make([]byte, 512)
	copy(host, hostPrefix)
	copy(host[len(hostPrefix):], bytes.Repeat([]byte{0xcc}, 512-len(hostPrefix)))
	buf.Write(host)

	if withZip {
		var zbuf bytes.Buffer
		zw := zip.NewWriter(&zbuf)
		w, err := zw.Create("hello.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("hello sfx")); err != nil {
			t.Fatal(err)
		}
		w, err = zw.Create("sub/data.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte{1, 2, 3, 4}); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		buf.Write(zbuf.Bytes())
	}

	path := filepath.Join(t.TempDir(), "app.exe")
	if err := os.WriteFile(path, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestZipSFXDetectAndList(t *testing.T) {
	path := buildSFXZip(t, []byte("MZ"), true)

	format, err := Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if format != "zip" {
		t.Fatalf("Detect = %q, want zip", format)
	}

	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entries, err := a.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["hello.txt"] || !names["sub/data.bin"] {
		t.Fatalf("missing entries, got %v", names)
	}

	rc, _, err := a.Open("hello.txt")
	if err != nil {
		t.Fatalf("Open hello.txt: %v", err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "hello sfx" {
		t.Fatalf("hello.txt = %q", data)
	}
}

func TestZipSFXELFHost(t *testing.T) {
	path := buildSFXZip(t, []byte{0x7f, 'E', 'L', 'F'}, true)
	format, err := Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if format != "zip" {
		t.Fatalf("Detect = %q, want zip", format)
	}
}

func TestZipSFXNegative(t *testing.T) {
	// A native binary without a ZIP overlay must not be detected.
	path := buildSFXZip(t, []byte("MZ"), false)
	format, err := Detect(path)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if format != "" {
		t.Fatalf("Detect = %q, want empty", format)
	}
}
