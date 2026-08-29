package image

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// writeTestTar builds (in memory, no external tool) a tarball holding a
// directory, a regular file, a symlink and a hardlink, and writes it to dir.
func writeTestTar(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "test.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)

	entries := []struct {
		hdr  *tar.Header
		data string
	}{
		{&tar.Header{Name: "sub/", Typeflag: tar.TypeDir, Mode: 0o755}, ""},
		{&tar.Header{Name: "sub/a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5}, "hello"},
		{&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "sub/a.txt"}, ""},
		{&tar.Header{Name: "hl", Typeflag: tar.TypeLink, Linkname: "sub/a.txt"}, ""},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(e.hdr); err != nil {
			t.Fatal(err)
		}
		if e.data != "" {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlainTarListReadExtract(t *testing.T) {
	path := writeTestTar(t, t.TempDir())

	if c := ClassifyInput(path); c != "archive" {
		t.Fatalf("ClassifyInput = %q, want archive", c)
	}

	entries, err := ListPlainArchive(path, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Fatalf("sub listing = %+v", entries)
	}

	// Symlink is followed to the target file.
	rc, _, err := ReadPlainArchiveFile(path, "link")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(data) != "hello" {
		t.Fatalf("link content = %q (err %v)", data, err)
	}

	dest := t.TempDir()
	if _, err := ExtractPlainArchive(path, dest, 4096); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "sub", "a.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("a.txt = %q (err %v)", b, err)
	}
	if link, err := os.Readlink(filepath.Join(dest, "link")); err != nil || link != "sub/a.txt" {
		t.Fatalf("link -> %q (err %v)", link, err)
	}
	// Hardlink is materialized with the target content.
	if b, err := os.ReadFile(filepath.Join(dest, "hl")); err != nil || string(b) != "hello" {
		t.Fatalf("hl = %q (err %v)", b, err)
	}
}
