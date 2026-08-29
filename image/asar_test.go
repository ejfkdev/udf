package image

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// writeTestASAR builds a minimal valid ASAR holding a.txt ("aaa"),
// sub/n.txt ("bc") and a symlink l -> a.txt, returning its path.
func writeTestASAR(t *testing.T, dir string) string {
	t.Helper()
	header := `{"files":{"a.txt":{"offset":"0","size":3},"sub":{"files":{"n.txt":{"offset":"3","size":2}}},"l":{"link":"a.txt"}}}`
	jb := []byte(header)
	aligned := (len(jb) + 3) &^ 3

	var hb []byte
	hb = binary.LittleEndian.AppendUint32(hb, uint32(4+aligned)) // pickle payload size
	hb = binary.LittleEndian.AppendUint32(hb, uint32(len(jb)))   // JSON string length
	hb = append(hb, jb...)
	hb = append(hb, make([]byte, aligned-len(jb))...)

	var sp []byte
	sp = binary.LittleEndian.AppendUint32(sp, 4)               // size-pickle payload size
	sp = binary.LittleEndian.AppendUint32(sp, uint32(len(hb))) // header length

	out := append(append(append([]byte{}, sp...), hb...), []byte("aaabc")...)
	path := filepath.Join(dir, "test.asar")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestASARListAndOpen(t *testing.T) {
	path := writeTestASAR(t, t.TempDir())

	if c := ClassifyInput(path); c != "archive" {
		t.Fatalf("ClassifyInput = %q, want archive", c)
	}

	entries, err := ListPlainArchive(path, "")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]FileEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	for _, want := range []string{"a.txt", "sub", "l"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("missing %q in listing: %v", want, byName)
		}
	}
	if e := byName["a.txt"]; e.Type != "file" || e.Size != 3 {
		t.Fatalf("a.txt = %+v", e)
	}
	if e := byName["sub"]; e.Type != "dir" {
		t.Fatalf("sub = %+v", e)
	}
	if e := byName["l"]; e.Type != "symlink" || e.Target != "a.txt" {
		t.Fatalf("l = %+v", e)
	}

	sub, err := ListPlainArchive(path, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) != 1 || sub[0].Name != "n.txt" || sub[0].Size != 2 {
		t.Fatalf("sub listing = %+v", sub)
	}

	rc, size, err := ReadPlainArchiveFile(path, "sub/n.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(data) != "bc" || size != 2 {
		t.Fatalf("n.txt = %q (size %d, err %v)", data, size, err)
	}

	// Symlink resolution through the plain-archive read path.
	rc2, _, err := ReadPlainArchiveFile(path, "l")
	if err != nil {
		t.Fatal(err)
	}
	data2, err := io.ReadAll(rc2)
	rc2.Close()
	if err != nil || string(data2) != "aaa" {
		t.Fatalf("l content = %q (err %v)", data2, err)
	}

	info, err := PlainArchiveInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "asar" || info.Files != 2 || info.TotalSize != 5 {
		t.Fatalf("info = %+v", info)
	}
}

func TestASARExtract(t *testing.T) {
	path := writeTestASAR(t, t.TempDir())
	dest := filepath.Join(t.TempDir(), "out")

	if _, err := ExtractPlainArchive(path, dest, 4096); err != nil {
		t.Fatal(err)
	}

	if b, err := os.ReadFile(filepath.Join(dest, "a.txt")); err != nil || string(b) != "aaa" {
		t.Fatalf("a.txt = %q (err %v)", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "sub", "n.txt")); err != nil || string(b) != "bc" {
		t.Fatalf("n.txt = %q (err %v)", b, err)
	}
	if link, err := os.Readlink(filepath.Join(dest, "l")); err != nil || link != "a.txt" {
		t.Fatalf("l -> %q (err %v)", link, err)
	}
}
