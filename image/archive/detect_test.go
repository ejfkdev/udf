package archive

import (
	"os"
	"path/filepath"
	"testing"
)

// A single compressed file (not wrapping a tar or cpio) must not be classified
// as a supported archive, because the archive pipeline cannot list it.
func TestSingleFileCompressionNotDetected(t *testing.T) {
	raw := []byte("just a single file, not a tar or cpio\n")
	for _, c := range []string{"gzip", "zstd", "xz", "lz4"} {
		p := filepath.Join(t.TempDir(), "sample."+c)
		if err := os.WriteFile(p, compressRaw(t, c, raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if f, _ := Detect(p); f != "" {
			t.Fatalf("Detect(%s single file) = %q, want empty", c, f)
		}
	}
}

// "070707" is the old ODC portable cpio variant (octal fields); the cpio
// reader only parses the newc/crc "070701"/"070702" hex form, so it must not
// be reported as a readable cpio.
func TestODCCpioNotDetected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sample.cpio.odc")
	if err := os.WriteFile(p, []byte("0707077777770000"), 0o644); err != nil {
		t.Fatal(err)
	}
	if f, _ := Detect(p); f == "cpio" {
		t.Fatalf("Detect(ODC cpio) = cpio, want unsupported")
	}
}
