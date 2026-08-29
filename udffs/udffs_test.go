package udffs

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDecodeName8Bit(t *testing.T) {
	// compression ID 8: bytes after the leading 8 are Latin-1 / ASCII.
	if got := decodeName([]byte{8, 'h', 'i'}); got != "hi" {
		t.Fatalf("decodeName(8) = %q", got)
	}
}

func TestDecodeNameUTF16(t *testing.T) {
	// compression ID 16: big-endian UTF-16.
	name := []byte{16, 0, 'a', 0, 'b'}
	if got := decodeName(name); got != "ab" {
		t.Fatalf("decodeName(16) = %q", got)
	}
}

func TestDecodeSymlink(t *testing.T) {
	// An absolute symlink with two path components ("usr", "bin").
	comp := func(typ byte, name string) []byte {
		nb := append([]byte{8}, name...)
		c := []byte{typ, byte(len(nb)), 0, 0}
		return append(c, nb...)
	}
	b := append(comp(2, ""), append(comp(5, "usr"), comp(5, "bin")...)...)
	if got := decodeSymlink(b); got != "/usr/bin" {
		t.Fatalf("decodeSymlink = %q", got)
	}
}

func buildFE(t *testing.T, fileType uint8, flags uint16, perm uint32, infoLen uint64, ads []byte) []byte {
	t.Helper()
	fe := make([]byte, 176+len(ads))
	le := binary.LittleEndian
	le.PutUint16(fe[0:2], tagFE)
	fe[27] = fileType
	le.PutUint16(fe[34:36], flags)
	le.PutUint32(fe[44:48], perm)
	le.PutUint16(fe[48:50], 1)
	le.PutUint64(fe[56:64], infoLen)
	le.PutUint32(fe[168:172], 0)                // lenEA
	le.PutUint32(fe[172:176], uint32(len(ads))) // lenAD
	copy(fe[176:], ads)
	return fe
}

func TestParseFEAndMode(t *testing.T) {
	ad := make([]byte, 8)
	le := binary.LittleEndian
	le.PutUint32(ad[0:4], 100) // extent length
	le.PutUint32(ad[4:8], 7)   // extent lbn

	fe := buildFE(t, ftRegular, adShort, 0x1884, 100, ad) // owner rw + group r + other r
	parsed, ok := parseFE(fe)
	if !ok {
		t.Fatal("parseFE failed")
	}
	if parsed.fileType != ftRegular {
		t.Fatalf("fileType = %d", parsed.fileType)
	}
	if parsed.mode() != fs.FileMode(0o644) {
		t.Fatalf("mode = %#o, want 0o644", parsed.mode())
	}
	if parsed.infoLen != 100 {
		t.Fatalf("infoLen = %d", parsed.infoLen)
	}
	exts := parsed.extents()
	if len(exts) != 1 || exts[0].lbn != 7 || exts[0].length != 100 {
		t.Fatalf("extents = %+v", exts)
	}
}

func TestParseEFE(t *testing.T) {
	// Build a minimal Extended File Entry (216-byte header) with one long AD.
	fe := make([]byte, 216+16)
	le := binary.LittleEndian
	le.PutUint16(fe[0:2], tagEFE)
	fe[27] = ftDirectory
	le.PutUint16(fe[34:36], adLong)
	le.PutUint64(fe[56:64], 5)
	le.PutUint32(fe[216:220], 32) // long AD: length
	le.PutUint32(fe[220:224], 9)  // long AD: lbn
	le.PutUint32(fe[208:212], 0)  // lenEA
	le.PutUint32(fe[212:216], 16) // lenAD

	parsed, ok := parseFE(fe)
	if !ok {
		t.Fatal("parseEFE failed")
	}
	if parsed.fileType != ftDirectory {
		t.Fatalf("fileType = %d", parsed.fileType)
	}
	exts := parsed.extents()
	if len(exts) != 1 || exts[0].lbn != 9 || exts[0].length != 32 {
		t.Fatalf("extents = %+v", exts)
	}
}

func TestDetectRejectsNonUDF(t *testing.T) {
	if Detect(bytes.NewReader(make([]byte, 512*2048)), 0) {
		t.Fatal("Detect accepted a non-UDF image")
	}
}

// TestRoundTripHdiutil validates the reader against an image produced by
// Apple's hdiutil. It is skipped unless UDF_VERIFY is set (and hdiutil is
// available on macOS); it creates its own fixture so no external file is
// required.
func TestRoundTripHdiutil(t *testing.T) {
	if os.Getenv("UDF_VERIFY") == "" {
		t.Skip("set UDF_VERIFY=1 to run the hdiutil round-trip")
	}
	if _, err := exec.LookPath("hdiutil"); err != nil {
		t.Skip("hdiutil not available")
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello UDF\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "nested.txt"), []byte("nested file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "image") // hdiutil appends ".iso"
	if b, err := exec.Command("hdiutil", "makehybrid", "-udf", "-udf-version", "1.50",
		"-udf-volume-name", "TESTVOL", "-only-udf", "*", "-o", out, dir).CombinedOutput(); err != nil {
		t.Fatalf("hdiutil makehybrid: %v: %s", err, b)
	}
	iso := out + ".iso"

	fh, err := os.Open(iso)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()

	if !Detect(fh, 0) {
		t.Fatal("Detect returned false")
	}
	fsys, err := Open(fh, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ents, err := fsys.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir root: %v", err)
	}
	names := map[string]bool{}
	for _, e := range ents {
		names[e.Name()] = true
	}
	for _, want := range []string{"hello.txt", "sub"} {
		if !names[want] {
			t.Fatalf("missing %q in root listing", want)
		}
	}

	f, err := fsys.Open("hello.txt")
	if err != nil {
		t.Fatalf("Open hello.txt: %v", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read hello.txt: %v", err)
	}
	if string(data) != "hello UDF\n" {
		t.Fatalf("hello.txt = %q", data)
	}
	f.Close()

	f2, err := fsys.Open("sub/nested.txt")
	if err != nil {
		t.Fatalf("Open sub/nested.txt: %v", err)
	}
	d2, _ := io.ReadAll(f2)
	if string(d2) != "nested file\n" {
		t.Fatalf("nested.txt = %q", d2)
	}
	f2.Close()
}
