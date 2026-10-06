package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	arch "github.com/ejfkdev/udf/image/archive"
)

// fixedModTime keeps the fixture deterministic.
var fixedModTime = time.Unix(1700000000, 0).UTC()

func md5Of(t *testing.T, r io.Reader) string {
	t.Helper()
	h := md5.New()
	if _, err := io.Copy(h, r); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func md5Bytes(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// buildBigPlainTarGz writes a gzip tar large enough for the random-access
// index to apply (the gate is the compressed size), with the member shapes a
// real bundle has: deep directories, a large incompressible payload, a
// symlink and a hard link.
func buildBigPlainTarGz(t *testing.T) (string, map[string][]byte, map[string]string) {
	t.Helper()
	const payloadSize = 66 << 20 // incompressible, so the archive stays above the gate

	payload := make([]byte, payloadSize)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	small := []byte("hello from a plain archive\n")
	deep := []byte("deep file contents")

	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)

	write := func(name string, body []byte, mode int64) {
		hdr := &tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg, Uid: 1000, Gid: 1000, ModTime: fixedModTime}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "data/", Mode: 0o755, Typeflag: tar.TypeDir, ModTime: fixedModTime}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "data/deep/deeper/", Mode: 0o750, Typeflag: tar.TypeDir, ModTime: fixedModTime}); err != nil {
		t.Fatal(err)
	}
	write("data/deep/deeper/blob.bin", payload, 0o600)
	write("README.txt", small, 0o644)
	write("data/deep/note.txt", deep, 0o644)
	if err := tw.WriteHeader(&tar.Header{Name: "readme-link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "README.txt", ModTime: fixedModTime}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "readme-hard", Mode: 0o644, Typeflag: tar.TypeLink, Linkname: "README.txt", Size: 0, ModTime: fixedModTime}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"data/deep/deeper/blob.bin": payload,
		"README.txt":                small,
		"data/deep/note.txt":        deep,
	}
	links := map[string]string{"readme-link": "README.txt", "readme-hard": "README.txt"}
	return path, files, links
}

func TestPlainArchiveUsesIndex(t *testing.T) {
	path, files, links := buildBigPlainTarGz(t)

	// The index applies and gets built on first use.
	src, ok := openIndexedPlainArchive(path)
	if !ok {
		t.Fatal("the index did not apply to a 66 MB gzip tar")
	}
	if src.source != "index" {
		t.Fatalf("source = %q", src.source)
	}
	entries, err := src.ar.List()
	if err != nil {
		t.Fatal(err)
	}
	if err := src.close(); err != nil {
		t.Fatal(err)
	}

	// The same members, with the same header fields, as a sequential read.
	seq, err := arch.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := seq.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Fatalf("index listed %d members, the archive has %d", len(entries), len(want))
	}
	byName := map[string]arch.Entry{}
	for i := range want {
		byName[want[i].Name] = want[i]
	}
	for _, e := range entries {
		w, ok := byName[e.Name]
		if !ok {
			t.Fatalf("index has member %q the archive does not", e.Name)
		}
		if e.Size != w.Size || e.Kind != w.Kind || e.Mode != w.Mode {
			t.Errorf("member %q: index has size=%d kind=%v mode=%o, archive has size=%d kind=%v mode=%o",
				e.Name, e.Size, e.Kind, e.Mode, w.Size, w.Kind, w.Mode)
		}
		if e.Linkname != w.Linkname {
			t.Errorf("member %q: linkname %q != %q", e.Name, e.Linkname, w.Linkname)
		}
		if e.UID != w.UID || e.GID != w.GID {
			t.Errorf("member %q: uid/gid %d/%d != %d/%d", e.Name, e.UID, e.GID, w.UID, w.GID)
		}
	}

	// Listing and reading through the public API, index and all.
	list, err := ListPlainArchive(path, "/data/deep/deeper")
	if err != nil {
		t.Fatalf("ListPlainArchive: %v", err)
	}
	if len(list) != 1 || list[0].Name != "blob.bin" || list[0].Size != int64(len(files["data/deep/deeper/blob.bin"])) {
		t.Fatalf("unexpected listing: %+v", list)
	}
	rc, size, err := ReadPlainArchiveFile(path, "/README.txt")
	if err != nil {
		t.Fatalf("ReadPlainArchiveFile: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, files["README.txt"]) || size != int64(len(files["README.txt"])) {
		t.Fatalf("README.txt read back as %q (%d bytes)", got, size)
	}

	// A member read through the index must be byte-identical to the archive's
	// (the first bytes change on every run, so only the tail is compared for
	// the large payload).
	big := "data/deep/deeper/blob.bin"
	rc, size, err = ReadPlainArchiveFile(path, "/"+big)
	if err != nil {
		t.Fatalf("read the payload: %v", err)
	}
	sum := md5Of(t, rc)
	rc.Close()
	if size != int64(len(files[big])) {
		t.Fatalf("payload size %d, want %d", size, len(files[big]))
	}
	if want := md5Bytes(files[big]); sum != want {
		t.Fatalf("payload md5 %s, want %s", sum, want)
	}

	// Symlink and hard link resolve to the file they point at.
	for name, target := range links {
		rc, _, err := ReadPlainArchiveFile(path, "/"+name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, files[target]) {
			t.Fatalf("%s did not resolve to %s", name, target)
		}
	}

	// The info summary matches the sequential one.
	idxInfo, err := PlainArchiveInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	if idxInfo.Format != "tar.gz" || idxInfo.Files != 3 {
		t.Fatalf("unexpected info: %+v", idxInfo)
	}
	wantTotal := int64(0)
	for _, b := range files {
		wantTotal += int64(len(b))
	}
	if idxInfo.TotalSize != wantTotal {
		t.Fatalf("total size %d, want %d", idxInfo.TotalSize, wantTotal)
	}
}
