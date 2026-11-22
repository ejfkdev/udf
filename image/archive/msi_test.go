package archive

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ejfkdev/udf/fsview"
)

// The fixture is an MSI written by wixl (msitools), embedding one MSZIP
// cabinet with two files. Its decoded stream names and member contents were
// cross-checked with msiinfo/msiextract before being checked in.
const msiFixture = "testdata/test.msi"

func openMSIFixture(t *testing.T) *msiArchive {
	t.Helper()
	a, err := openMSI(msiFixture)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return a.(*msiArchive)
}

func TestMSIListing(t *testing.T) {
	a := openMSIFixture(t)
	entries, err := a.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byName := map[string]Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}

	// Installer tables surface under their decoded names (no U+4840 marker).
	for _, want := range []string{"Property", "Directory", "Component", "Feature",
		"FeatureComponents", "Media", "File", "InstallExecuteSequence",
		"_StringPool", "_StringData", "_Tables", "_Columns"} {
		e, ok := byName[want]
		if !ok {
			t.Fatalf("table stream %s missing from the listing", want)
		}
		if e.Kind != fsview.KindFile {
			t.Fatalf("%s kind %v, want file", want, e.Kind)
		}
		if e.Size == 0 {
			t.Fatalf("%s has no data", want)
		}
	}

	// The embedded cabinet: the raw stream stays a file entry (size = the cab
	// bytes), and its members list as child paths the tree layer renders as a
	// directory.
	if e, ok := byName["data1.cab"]; !ok || e.Kind != fsview.KindFile || e.Size != 143 {
		t.Fatalf("data1.cab missing or not the raw stream: %+v", byName["data1.cab"])
	}
	for _, want := range []string{"data1.cab/ReadmeFile", "data1.cab/RunFile"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("cab member %s missing from the listing", want)
		}
	}
}

func TestMSIReading(t *testing.T) {
	a := openMSIFixture(t)

	// A cab member's bytes must be the source file it was built from.
	rc, size, err := a.Open("data1.cab/RunFile")
	if err != nil {
		t.Fatalf("open cab member: %v", err)
	}
	body, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) != size || size != 18 {
		t.Fatalf("member size %d (len %d), want 18", size, len(body))
	}
	if sum := md5.Sum(body); hex.EncodeToString(sum[:]) != "46bbbe8aa98cc0714426e948474eaaf4" {
		t.Fatalf("member md5 %x", sum)
	}

	// A table stream reads as its raw bytes (MSI tables are string-pool
	// encoded, so only the size is asserted).
	rc, size, err = a.Open("Property")
	if err != nil {
		t.Fatalf("open Property stream: %v", err)
	}
	defer rc.Close()
	buf := make([]byte, size)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatal(err)
	}
	if size != 24 {
		t.Fatalf("Property stream size %d, want 24", size)
	}

	if _, _, err := a.Open("no/such/stream"); err == nil {
		t.Fatal("opening a missing stream succeeded")
	}
}

func TestMSIExtraction(t *testing.T) {
	a := openMSIFixture(t)
	rc, _, err := a.Open("data1.cab/RunFile")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	// Extraction writes the entry mode; the default for streams is 0644 so a
	// copied member stays readable.
	entries, err := a.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "data1.cab/RunFile" && e.Mode != 0o644 {
			t.Fatalf("member mode %o, want 644", e.Mode)
		}
	}
}

func TestMSINameDecoding(t *testing.T) {
	// Compressed: U+4840 prefix + pairs packed into 0x3800..0x47FF units.
	// "File" as written by wixl in the fixture: 0x4840, 0x430F, 0x422F.
	got := decodeStreamName([]uint16{0x4840, 0x430F, 0x422F})
	if got != "File" {
		t.Fatalf("decoded %q, want File", got)
	}
	// Plain names pass through.
	if got := decodeStreamName([]uint16{'S', 'u', 'm'}); got != "Sum" {
		t.Fatalf("plain name decoded to %q", got)
	}
}

func TestCFBRejectsGarbage(t *testing.T) {
	// A file with the magic but a nonsense sector shift must be rejected.
	buf := make([]byte, 4096)
	copy(buf, cfbMagic)
	binary.LittleEndian.PutUint16(buf[28:], 0xFFFE) // valid byte order
	binary.LittleEndian.PutUint16(buf[30:], 7)      // invalid sector shift
	f := filepath.Join(t.TempDir(), "x.cfb")
	if err := os.WriteFile(f, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	fp, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer fp.Close()
	if _, err := openCFB(fp); err == nil {
		t.Fatal("a corrupt compound file was accepted")
	}
	if _, err := openCFB(fp); err == nil || !strings.Contains(err.Error(), "sector shift") {
		t.Fatalf("unexpected error: %v", err)
	}
}
