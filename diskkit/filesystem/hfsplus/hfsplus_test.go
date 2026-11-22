package hfsplus

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openBare opens the checked-in fixture: a 2 MiB HFS+ volume hdiutil wrote
// (-layout NONE), holding a.txt ("tiny dmg\n"), d/b.txt ("nested\n") and a
// symlink link -> a.txt. It was decoded from a DMG by an independent reader
// and verified mountable by macOS before being checked in.
func openBare(t *testing.T) *Volume {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "hfs-bare.img"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	v, err := Open(f, 0, 2<<20)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return v
}

func TestVolumeMetadata(t *testing.T) {
	v := openBare(t)
	if v.volumeName != "T" {
		t.Fatalf("volume name %q, want %q", v.volumeName, "T")
	}
	if v.blockSize != 4096 {
		t.Fatalf("block size %d, want 4096", v.blockSize)
	}
}

func TestRootListing(t *testing.T) {
	v := openBare(t)
	entries, err := v.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"a.txt", "d", "link"}
	if len(names) != len(want) {
		t.Fatalf("root lists %v, want %v", names, want)
	}
	for _, w := range want {
		found := false
		for _, n := range names {
			if n == w {
				found = true
			}
		}
		if !found {
			t.Fatalf("root is missing %s: %v", w, names)
		}
	}

	// The HFS+ private directories must never surface.
	for _, e := range entries {
		if strings.Contains(e.Name(), "HFS+ Private") {
			t.Fatalf("private directory surfaced: %s", e.Name())
		}
	}
}

func TestStatSemantics(t *testing.T) {
	v := openBare(t)

	info, err := v.Stat("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len("tiny dmg\n")) || info.IsDir() || info.Mode().IsRegular() != true {
		t.Fatalf("a.txt stat: size=%d mode=%v", info.Size(), info.Mode())
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("a.txt mode %v, want 0644", info.Mode().Perm())
	}

	d, err := v.Stat("d")
	if err != nil {
		t.Fatal(err)
	}
	if !d.IsDir() {
		t.Fatalf("d is not a directory: %v", d.Mode())
	}

	link, err := v.Stat("link")
	if err != nil {
		t.Fatal(err)
	}
	if link.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("link stat has no symlink type bits: %v", link.Mode())
	}
	if link.Size() != int64(len("a.txt")) {
		t.Fatalf("link size %d, want 5 (the target path)", link.Size())
	}

	if _, err := v.Stat("missing"); err == nil {
		t.Fatal("stat of a missing path succeeded")
	}
}

func TestReadlink(t *testing.T) {
	v := openBare(t)
	target, err := v.Readlink("link")
	if err != nil {
		t.Fatal(err)
	}
	if target != "a.txt" {
		t.Fatalf("link -> %q, want a.txt", target)
	}
	if _, err := v.Readlink("a.txt"); err == nil {
		t.Fatal("readlink of a regular file succeeded")
	}
}

func TestReadFiles(t *testing.T) {
	v := openBare(t)
	for name, want := range map[string]string{
		"a.txt":   "tiny dmg\n",
		"d/b.txt": "nested\n",
	} {
		f, err := v.Open(name)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		got, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestNestedListing walks d/ through ReadDir and Stat the way the disk
// extractor does.
func TestNestedListing(t *testing.T) {
	v := openBare(t)
	entries, err := v.ReadDir("d")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "b.txt" {
		t.Fatalf("d/ lists %v, want [b.txt]", entries)
	}
	info, err := v.Stat("d/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len("nested\n")) {
		t.Fatalf("d/b.txt size %d", info.Size())
	}
	if _, err := v.ReadDir("a.txt"); err == nil {
		t.Fatal("ReadDir of a file succeeded")
	}
}

// TestReadAheadReader exercises the file reader across the read-ahead window
// boundary with small reads, the shape io.CopyBuffer produces.
func TestReadAheadReader(t *testing.T) {
	v := openBare(t)
	f, err := v.Open("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got bytes.Buffer
	buf := make([]byte, 3)
	for {
		n, err := f.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if got.String() != "tiny dmg\n" {
		t.Fatalf("chunked read gave %q", got.String())
	}
}

func TestOpenRejectsNonHFS(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "hfs-bare.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Offset 1024 of the fixture is the volume header, so shifting the start
	// by one block lands on a data block: not a volume header.
	if _, err := Open(f, 4096, 2<<20); err == nil {
		t.Fatal("opened a non-HFS+ region as a volume")
	}
}

func md5sum(t *testing.T, r io.Reader) string {
	t.Helper()
	h := md5.New()
	if _, err := io.Copy(h, r); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
