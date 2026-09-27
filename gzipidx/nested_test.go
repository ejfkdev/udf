package gzipidx

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	kgzip "github.com/klauspost/compress/gzip"
)

// layerEntries is a small but varied layer: directories, files of padding-
// relevant sizes, a symlink, a hardlink, a name that needs a GNU long name and
// one written as a PAX header.
func layerEntries() []*tar.Header {
	long := bytes.Repeat([]byte("long-directory-name/"), 8)
	return []*tar.Header{
		{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatUSTAR},
		{Name: "etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: 511, Format: tar.FormatUSTAR},
		{Name: "etc/shadow", Typeflag: tar.TypeReg, Mode: 0o600, Size: 512, Format: tar.FormatUSTAR},
		{Name: "etc/hosts", Typeflag: tar.TypeReg, Mode: 0o644, Size: 513, Format: tar.FormatUSTAR},
		{Name: "etc/empty", Typeflag: tar.TypeReg, Mode: 0o644, Size: 0, Format: tar.FormatUSTAR},
		{Name: "bin/tool", Typeflag: tar.TypeReg, Mode: 0o755, Size: 100, Format: tar.FormatUSTAR},
		{Name: "bin/link", Typeflag: tar.TypeSymlink, Linkname: "tool", Mode: 0o777, Format: tar.FormatUSTAR},
		{Name: "bin/hard", Typeflag: tar.TypeLink, Linkname: "bin/tool", Mode: 0o755, Format: tar.FormatUSTAR},
		{Name: "etc/wh.motd", Typeflag: tar.TypeReg, Mode: 0, Size: 0, Format: tar.FormatUSTAR},
		{Name: string(long) + "file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 7, Format: tar.FormatGNU},
		{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatPAX},
		{Name: "usr/local/", Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatPAX},
		{Name: "usr/local/notes.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 29, Format: tar.FormatPAX,
			Uid: 1000, Gid: 1000, Uname: "builder", Gname: "builder",
			ModTime: time.Unix(1700000000, 0)},
		{Name: "large", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6 << 20, Format: tar.FormatUSTAR},
	}
}

func writeLayer(t *testing.T, headers []*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range headers {
		h := *hdr
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("write header %s: %v", h.Name, err)
		}
		if h.Size > 0 {
			body := bytes.Repeat([]byte{byte('a' + len(h.Name)%26)}, int(h.Size))
			if _, err := tw.Write(body); err != nil {
				t.Fatalf("write body %s: %v", h.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeOuterTar assembles an outer tar holding a plain file and two layers, and
// returns it gzipped the way a docker-save archive is.
func writeOuterTar(t *testing.T, dir string) (string, []byte, []byte) {
	t.Helper()
	layerA := writeLayer(t, layerEntries())
	layerB := writeLayer(t, layerEntries()[:6])

	var outer bytes.Buffer
	tw := tar.NewWriter(&outer)
	plain := []byte("not a tar at all, just text\n")
	if err := tw.WriteHeader(&tar.Header{Name: "readme.txt", Mode: 0o644, Size: int64(len(plain)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(plain); err != nil {
		t.Fatal(err)
	}
	for i, layer := range [][]byte{layerA, layerB} {
		name := []string{"a/layer.tar", "b/layer.tar"}[i]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(layer)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(layer); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "outer.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := kgzip.NewWriter(f)
	if _, err := zw.Write(outer.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path, layerA, layerB
}

// TestNestedCaptureMatchesArchiveTar checks that a captured member replays into
// exactly the headers archive/tar reads from the member's own bytes, and that
// the plain (non-tar) member is left alone.
func TestNestedCaptureMatchesArchiveTar(t *testing.T) {
	dir := t.TempDir()
	path, layerA, layerB := writeOuterTar(t, dir)

	captured := map[string][]*tar.Header{}
	ix, err := Build(path, BuildOptions{CandidateSpacing: 1 << 20, Nested: &NestedOptions{
		Members: func(_ string, size int64, head []byte) bool {
			return size >= 1024 && LooksLikeTarHeader(head)
		},
		Headers: func(name string, headers []*tar.Header) error {
			captured[name] = headers
			return nil
		},
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// The directory of the outer tar must be unaffected by the capture.
	if len(ix.Entries) != 3 {
		t.Fatalf("index directory has %d entries, want 3", len(ix.Entries))
	}
	if _, ok := captured["readme.txt"]; ok {
		t.Fatalf("a member that is not a tar stream was captured")
	}

	for name, want := range map[string][]byte{"a/layer.tar": layerA, "b/layer.tar": layerB} {
		headers, ok := captured[name]
		if !ok {
			t.Fatalf("%s was not captured", name)
		}
		tr := tar.NewReader(bytes.NewReader(want))
		var expect []*tar.Header
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			expect = append(expect, hdr)
		}
		if len(headers) != len(expect) {
			t.Fatalf("%s: captured %d headers, want %d", name, len(headers), len(expect))
		}
		for i := range expect {
			if !reflect.DeepEqual(headers[i], expect[i]) {
				t.Fatalf("%s: header %d differs\n got: %+v\nwant: %+v", name, i, headers[i], expect[i])
			}
		}
	}
}

// TestNestedCaptureGuards checks that members whose layout cannot be replayed
// from headers alone are refused instead of replayed wrong: a PAX size
// override, a GNU sparse member and a corrupt header block.
func TestNestedCaptureGuards(t *testing.T) {
	block := func(size int64, typeflag byte, body []byte) []byte {
		hdr := make([]byte, 512)
		copy(hdr[0:100], "member")
		writeOctal(hdr[100:108], 0o644)
		writeOctal(hdr[108:116], 0)
		writeOctal(hdr[116:124], 0)
		writeOctal(hdr[124:136], size)
		writeOctal(hdr[136:148], 0)
		for i := 148; i < 156; i++ {
			hdr[i] = ' '
		}
		hdr[156] = typeflag
		copy(hdr[257:], "ustar\x0000")
		sum := 0
		for _, b := range hdr {
			sum += int(b)
		}
		writeOctal(hdr[148:156], int64(sum))
		out := append([]byte(nil), hdr...)
		if len(body) > 0 {
			out = append(out, body...)
			if pad := (512 - len(body)%512) % 512; pad > 0 {
				out = append(out, make([]byte, pad)...)
			}
		}
		return out
	}
	regular := func(size int64, typeflag byte) []byte {
		return block(size, typeflag, make([]byte, size))
	}
	paxRecord := func(record string, size int64, typeflag byte) []byte {
		// A PAX member whose body is the record, followed by the member it
		// describes.
		return append(block(int64(len(record)), 'x', []byte(record)), block(size, typeflag, make([]byte, size))...)
	}

	cases := []struct {
		name  string
		bytes []byte
	}{
		{
			name:  "pax size override",
			bytes: paxRecord("11 size=99\n", 7, '0'),
		},
		{
			name:  "gnu sparse member",
			bytes: regular(0, 'S'),
		},
		{
			name:  "corrupt header",
			bytes: func() []byte { b := regular(0, '0'); b[0] = 'X'; return b }(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &nestedCapture{name: "layer.tar"}
			c.feed(tc.bytes)
			if !c.abort {
				t.Fatalf("capture was not refused")
			}
			if _, ok := parseCapture(c); ok {
				t.Fatalf("an aborted capture still parsed")
			}
		})
	}

	// A well-formed stream through the same code path replays fine, including
	// a PAX member whose record does not touch the layout.
	good := paxRecord("17 comment=hello\n", 1000, '0')
	c := &nestedCapture{name: "layer.tar"}
	c.feed(good)
	c.feed(make([]byte, 1024))
	if c.abort {
		t.Fatalf("a faithful stream was refused")
	}
	headers, ok := parseCapture(c)
	if !ok {
		t.Fatalf("replay failed")
	}
	if len(headers) != 1 || headers[0].Size != 1000 {
		t.Fatalf("unexpected headers: %+v", headers)
	}
}

// TestNestedCaptureBudget checks that a capture exceeding the budget is
// dropped rather than kept: the caller then reads the member through the index.
func TestNestedCaptureBudget(t *testing.T) {
	dir := t.TempDir()
	path, _, _ := writeOuterTar(t, dir)

	got := 0
	_, err := Build(path, BuildOptions{CandidateSpacing: 1 << 20, Nested: &NestedOptions{
		Members: func(_ string, size int64, head []byte) bool {
			return size >= 1024 && LooksLikeTarHeader(head)
		},
		Headers: func(string, []*tar.Header) error {
			got++
			return nil
		},
		Budget: 1,
	}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got != 0 {
		t.Fatalf("%d members were captured despite a one-byte budget", got)
	}
}

// writeOctal writes a tar numeric field in the octal form tar uses: digits
// followed by a terminator, which must not eat the most significant digit.
func writeOctal(field []byte, v int64) {
	last := len(field) - 1
	for i := last - 1; i >= 0; i-- {
		field[i] = byte('0' + v%8)
		v /= 8
	}
	field[last] = 0
}
