package archive

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	kgzip "github.com/klauspost/compress/gzip"

	"github.com/ejfkdev/udf/fsview"
)

// newcEntry is one member of a hand-written newc cpio archive.
type newcEntry struct {
	name string
	mode uint32 // raw Unix mode bits, type bits included
	body string
	link string // for symlinks: stored as the entry's data, per the format
	uid  int
	gid  int
}

// writeNewc writes a newc ("070701") cpio archive byte by byte, from the format
// itself rather than through a library: the point of the fixture is to be an
// independent check of the parser, and libraries disagree about how a symlink's
// target is stored (it lives in the entry's data, which the header has no field
// for).
func writeNewc(entries []newcEntry, trailer bool) []byte {
	var buf bytes.Buffer
	put := func(format string, v ...any) {
		fmt.Fprintf(&buf, format, v...)
	}
	for _, e := range entries {
		body := e.body
		if e.mode&0o170000 == 0o120000 {
			body = e.link
		}
		put("070701")
		put("%08X", 1) // inode
		put("%08X", e.mode)
		put("%08X", e.uid)
		put("%08X", e.gid)
		put("%08X", 1) // nlink
		put("%08X", 1700000000)
		put("%08X", len(body))
		put("%08X", 0) // devmajor
		put("%08X", 0) // devminor
		put("%08X", 0) // rdevmajor
		put("%08X", 0) // rdevminor
		put("%08X", len(e.name)+1)
		put("%08X", 0) // check
		buf.WriteString(e.name)
		buf.WriteByte(0)
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
		buf.WriteString(body)
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
	}
	if trailer {
		put("070701")
		put("%08X", 0)
		put("%08X", 0)
		put("%08X", 0)
		put("%08X", 0)
		put("%08X", 1)
		put("%08X", 0)
		put("%08X", 0)
		put("%08X", 0)
		put("%08X", 0)
		put("%08X", 0)
		put("%08X", 0)
		put("%08X", 11)
		put("%08X", 0)
		buf.WriteString("TRAILER!!!\x00\x00\x00")
	}
	return buf.Bytes()
}

// ramdiskCpio is the archive a boot image's ramdisk holds in these tests.
func ramdiskCpio() []byte {
	return writeNewc([]newcEntry{
		{name: "etc", mode: 0o040000 | 0o755},
		{name: "etc/passwd", mode: 0o100000 | 0o644, body: "root:x:0:0\n"},
		{name: "init", mode: 0o100000 | 0o755, body: "#!/bin/sh\n"},
		{name: "bin", mode: 0o040000 | 0o755},
		{name: "bin/sh", mode: 0o120000 | 0o777, link: "/init"},
		{name: "long-name-file.txt", mode: 0o100000 | 0o644, body: "hello ramdisk\n"},
	}, true)
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := kgzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeBootImage assembles a v0-v2 style boot image: a page-aligned header,
// then kernel, ramdisk and (for v2) dtb.
func writeBootImage(t *testing.T, headerVersion uint32, page int, kernel, ramdisk, dtb []byte) string {
	t.Helper()

	hdr := make([]byte, 1660)
	copy(hdr, bootMagic)
	le := binary.LittleEndian
	le.PutUint32(hdr[8:12], uint32(len(kernel)))
	le.PutUint32(hdr[12:16], 0x10008000)
	le.PutUint32(hdr[16:20], uint32(len(ramdisk)))
	le.PutUint32(hdr[20:24], 0x11000000)
	le.PutUint32(hdr[32:36], 0x10000100)
	le.PutUint32(hdr[36:40], uint32(page))
	le.PutUint32(hdr[40:44], headerVersion)
	copy(hdr[48:64], "test-board")
	copy(hdr[64:64+len("console=ttyMSM0")], "console=ttyMSM0")
	if headerVersion >= 2 {
		le.PutUint32(hdr[1648:1652], uint32(len(dtb)))
	}

	var buf bytes.Buffer
	writePadded := func(b []byte) {
		buf.Write(b)
		if pad := align(int64(page), int64(buf.Len())) - int64(buf.Len()); pad > 0 {
			buf.Write(make([]byte, pad))
		}
	}
	writePadded(hdr)
	writePadded(kernel)
	writePadded(ramdisk)
	if len(dtb) > 0 {
		writePadded(dtb)
	}

	path := filepath.Join(t.TempDir(), "boot.img")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBootImgListsComponentsAndRamdisk checks both levels: the components, and
// the files inside a gzip-compressed ramdisk cpio.
func TestBootImgListsComponentsAndRamdisk(t *testing.T) {
	kernel := bytes.Repeat([]byte{0x11}, 3000)
	dtb := bytes.Repeat([]byte{0x22}, 900)
	path := writeBootImage(t, 2, 2048, kernel, gzipBytes(t, ramdiskCpio()), dtb)

	format, err := Detect(path)
	if err != nil || format != "android-boot" {
		t.Fatalf("Detect = %q, %v", format, err)
	}
	a, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	entries, err := a.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byName := map[string]Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	for _, want := range []string{"kernel", "ramdisk", "dtb", "ramdisk/etc/passwd", "ramdisk/init", "ramdisk/bin/sh", "ramdisk/long-name-file.txt"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("entry %q missing from %v", want, entryNames(entries))
		}
	}
	if got := byName["kernel"].Size; got != int64(len(kernel)) {
		t.Fatalf("kernel size = %d, want %d", got, len(kernel))
	}
	if k := byName["ramdisk/bin/sh"].Kind; k != fsview.KindSymlink {
		t.Fatalf("ramdisk/bin/sh kind = %v", k)
	}
	if byName["ramdisk/bin/sh"].Linkname != "/init" {
		t.Fatalf("symlink target = %q", byName["ramdisk/bin/sh"].Linkname)
	}

	// Component and ramdisk-file reads both return the original bytes.
	rc, size, err := a.Open("kernel")
	if err != nil {
		t.Fatalf("open kernel: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if size != int64(len(kernel)) || !bytes.Equal(got, kernel) {
		t.Fatalf("kernel bytes differ (%d vs %d)", len(got), len(kernel))
	}
	rc, _, err = a.Open("ramdisk/etc/passwd")
	if err != nil {
		t.Fatalf("open ramdisk file: %v", err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if string(got) != "root:x:0:0\n" {
		t.Fatalf("ramdisk file bytes = %q", got)
	}
}

// TestBootImgUncompressedRamdisk covers a ramdisk stored as a plain cpio, and
// TestBootImgWeakMagicRejected covers the header validation that keeps the
// eight-character magic from matching unrelated binaries.
func TestBootImgUncompressedRamdisk(t *testing.T) {
	path := writeBootImage(t, 0, 4096, bytes.Repeat([]byte{1}, 100), ramdiskCpio(), nil)
	a, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	entries, err := a.List()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := entryIndex(entries, "ramdisk/init"); !ok {
		t.Fatalf("uncompressed ramdisk was not listed: %v", entryNames(entries))
	}
}

func TestBootImgWeakMagicRejected(t *testing.T) {
	// "ANDROID!" followed by nonsense: plausible magic, implausible header.
	bad := make([]byte, 4096)
	copy(bad, bootMagic)
	binary.LittleEndian.PutUint32(bad[36:40], 7) // page size not a multiple of 512
	path := filepath.Join(t.TempDir(), "notboot.img")
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if format, err := Detect(path); err != nil || format == "android-boot" {
		t.Fatalf("Detect = %q, %v; a bogus header was accepted", format, err)
	}
	if _, err := Open(path); err == nil {
		t.Fatalf("Open accepted a bogus header")
	}
}

func entryNames(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func entryIndex(entries []Entry, name string) (Entry, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}
