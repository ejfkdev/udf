package archive

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ejfkdev/udf/fsview"
)

// narPkt appends one length-prefixed NAR byte packet to b.
func narPkt(b *bytes.Buffer, s string) {
	var lb [8]byte
	binary.LittleEndian.PutUint64(lb[:], uint64(len(s)))
	b.Write(lb[:])
	b.WriteString(s)
	if pad := (8 - len(s)%8) % 8; pad > 0 {
		b.Write(make([]byte, pad))
	}
}

// buildNAR crafts a small Nix archive: a root directory holding hello.txt, an
// executable tool and a symlink.
func buildNAR(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	p := func(s string) { narPkt(&b, s) }

	p("nix-archive-1")
	p("(") // root node
	p("type")
	p("directory")
	// entry hello.txt
	p("entry")
	p("(")
	p("name")
	p("hello.txt")
	p("node")
	p("(")
	p("type")
	p("regular")
	p("contents")
	p("hello")
	p(")")
	p(")")
	// entry tool (executable)
	p("entry")
	p("(")
	p("name")
	p("tool")
	p("node")
	p("(")
	p("type")
	p("regular")
	p("executable")
	p("") // empty string field
	p("contents")
	p("#!/bin/sh")
	p(")")
	p(")")
	// entry link -> hello.txt
	p("entry")
	p("(")
	p("name")
	p("link")
	p("node")
	p("(")
	p("type")
	p("symlink")
	p("target")
	p("hello.txt")
	p(")")
	p(")")
	p(")") // close root

	path := filepath.Join(t.TempDir(), "test.nar")
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNARListOpen(t *testing.T) {
	path := buildNAR(t)
	if f, err := Detect(path); err != nil || f != "nar" {
		t.Fatalf("Detect = %q, %v; want nar", f, err)
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
	if e, ok := byName["hello.txt"]; !ok || e.Kind != fsview.KindFile || e.Size != 5 {
		t.Fatalf("hello.txt = %+v (ok %v)", e, ok)
	}
	if e, ok := byName["tool"]; !ok || e.Kind != fsview.KindFile || e.Mode != 0o755 {
		t.Fatalf("tool = %+v (ok %v)", e, ok)
	}
	if e, ok := byName["link"]; !ok || e.Kind != fsview.KindSymlink || e.Linkname != "hello.txt" {
		t.Fatalf("link = %+v (ok %v)", e, ok)
	}

	rc, size, err := ar.Open("tool")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(b) != "#!/bin/sh" || size != 9 {
		t.Fatalf("tool = %q (size %d, err %v)", b, size, err)
	}
}
