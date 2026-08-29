package image

import (
	"bytes"
	"errors"
	"io"
	"testing"

	appi18n "github.com/ejfkdev/udf/i18n"
)

// catBinaryBlob deliberately mixes NUL, invalid-UTF-8 and control bytes to
// prove that cat round-trips arbitrary binary data untouched.
var catBinaryBlob = []byte{0x00, 0x01, 0x02, 0x7F, 0x80, 0xFE, 0xFF, 'A', '\n', 0x00, '\r', 0x1A}

func readAllClose(t *testing.T, rc io.ReadCloser) []byte {
	t.Helper()
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read content: %v", err)
	}
	return data
}

func TestReadArchiveFileBytes(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgDir("bin"),
			imgFile("bin/blob", string(catBinaryBlob)),
		},
	})

	rc, size, err := ReadArchiveFile(f.ImagePath, f.Meta, "/bin/blob")
	if err != nil {
		t.Fatalf("read archive file: %v", err)
	}
	data := readAllClose(t, rc)
	if !bytes.Equal(data, catBinaryBlob) {
		t.Fatalf("content mismatch:\n got %v\nwant %v", data, catBinaryBlob)
	}
	if size != int64(len(catBinaryBlob)) {
		t.Fatalf("size mismatch: got %d want %d", size, len(catBinaryBlob))
	}
}

func TestReadArchiveFileFollowsHardlink(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgDir("lib"),
			imgFile("lib/real.txt", "data"),
			imgHardlink("lib/linked.txt", "/lib/real.txt"),
		},
	})

	rc, _, err := ReadArchiveFile(f.ImagePath, f.Meta, "/lib/linked.txt")
	if err != nil {
		t.Fatalf("read hardlink: %v", err)
	}
	if data := readAllClose(t, rc); string(data) != "data" {
		t.Fatalf("unexpected hardlink content: %q", string(data))
	}
}

func TestReadArchiveFileFollowsSymlink(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgDir("etc"),
			imgFile("etc/passwd", "root:x:0:0\n"),
			imgDir("bin"),
			imgSymlink("bin/passwd", "../etc/passwd"),
		},
	})

	rc, _, err := ReadArchiveFile(f.ImagePath, f.Meta, "/bin/passwd")
	if err != nil {
		t.Fatalf("read symlink: %v", err)
	}
	if data := readAllClose(t, rc); string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected symlink content: %q", string(data))
	}
}

func TestReadArchiveFileFollowsAbsoluteSymlink(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {
			imgDir("etc"),
			imgFile("etc/passwd", "root:x:0:0\n"),
			imgDir("bin"),
			imgSymlink("bin/passwd", "/etc/passwd"),
		},
	})

	rc, _, err := ReadArchiveFile(f.ImagePath, f.Meta, "/bin/passwd")
	if err != nil {
		t.Fatalf("read absolute symlink: %v", err)
	}
	if data := readAllClose(t, rc); string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected absolute symlink content: %q", string(data))
	}
}

func TestReadArchiveFileNotFound(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {imgFile("etc/passwd", "x")},
	})

	_, _, err := ReadArchiveFile(f.ImagePath, f.Meta, "/nosuch")
	var le *appi18n.LocalizedError
	if !errors.As(err, &le) || le.Key != "err_cp_src_not_found" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadArchiveFileRejectsDirectory(t *testing.T) {
	f := buildTestImage(t, map[string][]layerTarEntry{
		"a.tar": {imgDir("etc"), imgFile("etc/passwd", "x")},
	})

	if _, _, err := ReadArchiveFile(f.ImagePath, f.Meta, "/etc"); err == nil {
		t.Fatal("expected error for directory source")
	}
}

func TestReadDiskFileBytes(t *testing.T) {
	rawPath := createExt4Image(t)
	qcow2Path := writeQCow2Image(t, mustReadFile(t, rawPath), 16)

	rc, size, err := ReadDiskFile(qcow2Path, "/etc/passwd")
	if err != nil {
		t.Fatalf("read disk file: %v", err)
	}
	data := readAllClose(t, rc)
	if string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected content: %q", string(data))
	}
	if size != int64(len("root:x:0:0\n")) {
		t.Fatalf("size mismatch: got %d", size)
	}
}

func TestReadDiskFileBareRawExt4(t *testing.T) {
	rawPath := createExt4Image(t)

	rc, _, err := ReadDiskFile(rawPath, "/etc/passwd")
	if err != nil {
		t.Fatalf("read bare raw disk file: %v", err)
	}
	if data := readAllClose(t, rc); string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected content: %q", string(data))
	}
}
