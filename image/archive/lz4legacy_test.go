package archive

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pierrec/lz4/v4"
)

// writeLegacyLZ4 builds an LZ4 legacy stream: the magic, then blocks of
// [u32 length][raw LZ4 block]. The compressor is the library's, so the reader
// is tested against bytes it did not produce.
func writeLegacyLZ4(t *testing.T, payloads ...[]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString(legacyLZ4Magic)
	for _, p := range payloads {
		comp := make([]byte, lz4.CompressBlockBound(len(p)))
		n, err := lz4.CompressBlock(p, comp, nil)
		if err != nil {
			t.Fatalf("CompressBlock: %v", err)
		}
		if n == 0 {
			t.Fatalf("compressor returned a stored block for %d bytes", len(p))
		}
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(n))
		out.Write(hdr[:])
		out.Write(comp[:n])
	}
	return out.Bytes()
}

func TestLegacyLZ4Reader(t *testing.T) {
	blockA := bytes.Repeat([]byte("alpha-block-"), 4096)
	blockB := []byte("short tail")
	stream := writeLegacyLZ4(t, blockA, blockB)

	// Through the shared decompressor, the way a ramdisk is read.
	got, err := openLegacyLZ4(bytes.NewReader(stream[4:])) // past the magic
	if err != nil {
		t.Fatalf("openLegacyLZ4: %v", err)
	}
	data, err := io.ReadAll(got)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := append(append([]byte{}, blockA...), blockB...)
	if !bytes.Equal(data, want) {
		t.Fatalf("decoded %d bytes, want %d", len(data), len(want))
	}

	// openCompressed sniffs the magic itself, so callers do not.
	f, err := os.CreateTemp(t.TempDir(), "legacy-*.lz4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(stream); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rf, err := os.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	r, closeFn, err := openCompressed(rf, "lz4")
	if err != nil {
		t.Fatalf("openCompressed: %v", err)
	}
	defer closeFn()
	data2, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read via openCompressed: %v", err)
	}
	if !bytes.Equal(data2, want) {
		t.Fatal("openCompressed produced different bytes")
	}
}

func TestLegacyLZ4TrailingDataAndTruncation(t *testing.T) {
	block := bytes.Repeat([]byte("ramdisk payload "), 1024)
	stream := writeLegacyLZ4(t, block)

	// A bootconfig blob after the stream ends it cleanly, the way the emulator
	// appends one to its initrd.
	withTail := append(append([]byte{}, stream...),
		[]byte("androidboot.boot_devices=\"a003600.virtio_mmio\"\n#BOOTCONFIG\n")...)
	out, err := openLegacyLZ4(bytes.NewReader(withTail[4:]))
	if err != nil {
		t.Fatalf("openLegacyLZ4 with trailing data: %v", err)
	}
	data, err := io.ReadAll(out)
	if err != nil {
		t.Fatalf("read with trailing data: %v", err)
	}
	if !bytes.Equal(data, block) {
		t.Fatal("trailing data changed the decoded bytes")
	}

	// A stream that ends inside a block is an error, not a silent truncation.
	truncated := stream[:len(stream)-10]
	out, err = openLegacyLZ4(bytes.NewReader(truncated[4:]))
	if err != nil {
		return // failing at open is fine too
	}
	if _, err := io.ReadAll(out); err == nil {
		t.Fatal("a truncated stream should not decode silently")
	}
}

// TestLegacyLZ4RealFile compares against lz4(1) when a real ramdisk is
// provided: set UDF_LZ4_LEGACY to an Android ramdisk (the emulator's ramdisk.img
// or an AVD's initrd).
func TestLegacyLZ4RealFile(t *testing.T) {
	path := os.Getenv("UDF_LZ4_LEGACY")
	if path == "" {
		t.Skip("set UDF_LZ4_LEGACY to an Android ramdisk to run this test")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, closeFn, err := openCompressed(f, "lz4")
	if err != nil {
		t.Fatalf("openCompressed: %v", err)
	}
	defer closeFn()
	ours, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}

	// The ramdisk is a newc cpio: check its magic and its TRAILER, which a
	// correct decode must contain at the end of the last member.
	if !bytes.HasPrefix(ours, []byte("070701")) {
		t.Fatalf("decoded ramdisk does not start with the newc magic: %q", ours[:16])
	}
	if !bytes.Contains(ours[len(ours)-512:], []byte("TRAILER!!!")) {
		t.Fatal("decoded ramdisk has no cpio trailer")
	}

	if lz4bin, err := exec.LookPath("lz4"); err == nil {
		ref := filepath.Join(t.TempDir(), "reference")
		cmd := exec.Command(lz4bin, "-d", "-f", path, ref)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("lz4(1) could not decode the file (skipping cross-check): %s", strings.TrimSpace(string(out)))
			return
		}
		want, err := os.ReadFile(ref)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ours, want) {
			t.Fatalf("decoded %d bytes, lz4(1) produced %d", len(ours), len(want))
		}
		t.Logf("decoded %d bytes, identical to lz4(1)", len(ours))
	}
}
