package archive

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ejfkdev/udf/fsview"
)

type xarFileSpec struct {
	name string
	data []byte // nil for a symlink
	link string // symlink target
}

// buildXAR crafts a minimal xar holding the given files/symlinks, using zlib
// for both the TOC and the file data (matching macOS libxar's convention).
func buildXAR(t *testing.T, files ...xarFileSpec) string {
	t.Helper()

	var heap []byte
	var body strings.Builder
	offset := int64(0)
	for _, f := range files {
		if f.link != "" {
			fmt.Fprintf(&body, `<file><type>symlink</type><name>%s</name><link type="file">%s</link></file>`, f.name, f.link)
			continue
		}
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		if _, err := zw.Write(f.data); err != nil {
			t.Fatal(err)
		}
		_ = zw.Close()
		compressed := zb.Bytes()
		fmt.Fprintf(&body, `<file><type>file</type><name>%s</name><data><length>%d</length><offset>%d</offset><size>%d</size><encoding style="application/x-gzip"/></data></file>`, f.name, len(compressed), offset, len(f.data))
		heap = append(heap, compressed...)
		offset += int64(len(compressed))
	}
	toc := `<?xml version="1.0" encoding="UTF-8"?><xar><toc>` + body.String() + "</toc></xar>"

	var tzb bytes.Buffer
	tw := zlib.NewWriter(&tzb)
	if _, err := tw.Write([]byte(toc)); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	tocCompressed := tzb.Bytes()

	hdr := make([]byte, 28)
	copy(hdr[0:4], "xar!")
	binary.BigEndian.PutUint16(hdr[4:6], 28)
	binary.BigEndian.PutUint16(hdr[6:8], 1)
	binary.BigEndian.PutUint64(hdr[8:16], uint64(len(tocCompressed)))
	binary.BigEndian.PutUint64(hdr[16:24], uint64(len(toc)))
	binary.BigEndian.PutUint32(hdr[24:28], 1) // SHA-1

	out := append(append(hdr, tocCompressed...), heap...)
	path := filepath.Join(t.TempDir(), "test.xar")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestXARListOpen(t *testing.T) {
	path := buildXAR(t,
		xarFileSpec{name: "foo.txt", data: []byte("hello from xar")},
		xarFileSpec{name: "link", link: "foo.txt"},
	)

	if f, err := Detect(path); err != nil || f != "xar" {
		t.Fatalf("Detect = %q, %v; want xar", f, err)
	}
	ar, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ar.List()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if e, ok := byName["foo.txt"]; !ok || e.Kind != fsview.KindFile || e.Size != 14 {
		t.Fatalf("foo.txt = %+v (ok %v)", e, ok)
	}
	if e, ok := byName["link"]; !ok || e.Kind != fsview.KindSymlink || e.Linkname != "foo.txt" {
		t.Fatalf("link = %+v (ok %v)", e, ok)
	}

	rc, size, err := ar.Open("foo.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(b) != "hello from xar" || size != 14 {
		t.Fatalf("foo.txt = %q (size %d, err %v)", b, size, err)
	}
}
