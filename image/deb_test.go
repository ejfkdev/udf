package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

type tfe struct {
	h *tar.Header
	d string
}

func gzipTar(t *testing.T, entries ...tfe) []byte {
	t.Helper()
	var zb bytes.Buffer
	zw := gzip.NewWriter(&zb)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		if err := tw.WriteHeader(e.h); err != nil {
			t.Fatal(err)
		}
		if e.d != "" {
			if _, err := tw.Write([]byte(e.d)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return zb.Bytes()
}

// writeAr writes an ar archive with the given (name, data) members.
func writeAr(t *testing.T, path string, members ...[2]string) {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	for _, m := range members {
		name, data := m[0], []byte(m[1])
		hdr := make([]byte, 60)
		for i := range hdr {
			hdr[i] = ' '
		}
		copy(hdr[0:16], name)
		copy(hdr[16:28], "0")
		copy(hdr[28:34], "0")
		copy(hdr[34:40], "0")
		copy(hdr[40:48], "100644")
		copy(hdr[48:58], strconv.Itoa(len(data)))
		hdr[58], hdr[59] = '`', '\n'
		buf.Write(hdr)
		buf.Write(data)
		if len(data)%2 == 1 {
			buf.WriteByte('\n')
		}
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDEBListOpenExtract(t *testing.T) {
	data := gzipTar(t,
		tfe{&tar.Header{Name: "usr/bin/foo", Typeflag: tar.TypeReg, Mode: 0o755, Size: 4}, "bar\n"},
		tfe{&tar.Header{Name: "etc/config", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3}, "cfg"},
		tfe{&tar.Header{Name: "usr/bin/foobar", Typeflag: tar.TypeSymlink, Linkname: "foo"}, ""},
	)
	path := filepath.Join(t.TempDir(), "test.deb")
	writeAr(t, path,
		[2]string{"debian-binary", "2.0\n"},
		[2]string{"control.tar.gz", "metadata"},
		[2]string{"data.tar.gz", string(data)},
	)

	if c := ClassifyInput(path); c != "archive" {
		t.Fatalf("ClassifyInput = %q, want archive", c)
	}
	info, err := PlainArchiveInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "deb" {
		t.Fatalf("format = %q, want deb", info.Format)
	}

	bin, err := ListPlainArchive(path, "usr/bin")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]FileEntry{}
	for _, e := range bin {
		byName[e.Name] = e
	}
	if e, ok := byName["foo"]; !ok || e.Type != "file" || e.Size != 4 {
		t.Fatalf("usr/bin/foo = %+v (ok %v)", e, ok)
	}
	if e, ok := byName["foobar"]; !ok || e.Type != "symlink" || e.Target != "foo" {
		t.Fatalf("usr/bin/foobar = %+v (ok %v)", e, ok)
	}

	rc, size, err := ReadPlainArchiveFile(path, "usr/bin/foo")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(b) != "bar\n" || size != 4 {
		t.Fatalf("usr/bin/foo = %q (size %d, err %v)", b, size, err)
	}

	dest := t.TempDir()
	if _, err := ExtractPlainArchive(path, dest, 4096); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "usr", "bin", "foo")); err != nil || string(b) != "bar\n" {
		t.Fatalf("extracted foo = %q (err %v)", b, err)
	}
	if l, err := os.Readlink(filepath.Join(dest, "usr", "bin", "foobar")); err != nil || l != "foo" {
		t.Fatalf("foobar -> %q (err %v)", l, err)
	}
}
