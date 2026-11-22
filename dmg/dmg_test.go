package dmg

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures are 2 MiB HFS+ images written by hdiutil (macOS) in each UDIF
// compression format, holding the same files: a.txt, d/b.txt and a symlink.
// Their full virtual disks decode to these digests (computed independently of
// this package, by a Python reader of the same chunk tables).
//
// LZFSE and ADC digests are not listed: no independent Go-external decoder for
// them here, so they are checked structurally (below) and by content through
// the HFS+ reader in the image package.
var fullDiskDigests = map[string]string{
	"tiny-UDZO.dmg": "1aaaf281d7278afbb79d090250b30e4d",
	"tiny-UDBZ.dmg": "c35285d618267b4bfbf32e75898e5876",
	"tiny-ULMO.dmg": "c7f8b2afd1e1ec6671bab9962badc560",
	"tiny-UDRO.dmg": "ad07a10527238342648c9f75f802db93",
}

const (
	tinySize    = int64(4096) * Sector // every tiny image is a 2 MiB disk
	hfsAtSector = 40                   // GPT layout: the Apple_HFS partition starts here
)

func openFixture(t *testing.T, name string) *Disk {
	t.Helper()
	path := filepath.Join("testdata", name)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { f.Close() })
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	d, err := Open(f, st.Size())
	if err != nil {
		t.Fatalf("open dmg %s: %v", name, err)
	}
	if d.Size() != tinySize {
		t.Fatalf("%s: virtual size %d, want %d", name, d.Size(), tinySize)
	}
	return d
}

func TestOpenAndDecodeEveryFormat(t *testing.T) {
	for _, name := range []string{"tiny-UDZO.dmg", "tiny-UDBZ.dmg", "tiny-ULMO.dmg", "tiny-ULFO.dmg", "tiny-UDCO.dmg", "tiny-UDRO.dmg"} {
		t.Run(name, func(t *testing.T) {
			d := openFixture(t, name)

			// Sequential read of the whole virtual disk.
			full, err := io.ReadAll(io.NewSectionReader(d, 0, d.Size()))
			if err != nil {
				t.Fatalf("read whole disk: %v", err)
			}
			if want, ok := fullDiskDigests[name]; ok {
				if got := hex.EncodeToString(hashMD5(full)); got != want {
					t.Fatalf("decoded disk digest %s, want %s", got, want)
				}
			}

			// Random access must agree with the sequential bytes at the same
			// offsets: odd sizes, chunk-spanning and gap reads.
			for _, spec := range [][2]int64{
				{0, 512}, {Sector / 3, 1000}, {40 * Sector, 3 * Sector},
				{hfsAtSector*Sector + 1024, 512}, // HFS+ volume header, inside a compressed chunk
				{d.Size() - 100, 100}, {d.Size() - 1, 1},
			} {
				buf := make([]byte, spec[1])
				if _, err := d.ReadAt(buf, spec[0]); err != nil {
					t.Fatalf("ReadAt(%d, %d): %v", spec[0], spec[1], err)
				}
				if !bytes.Equal(buf, full[spec[0]:spec[0]+spec[1]]) {
					t.Fatalf("ReadAt(%d, %d) disagrees with the sequential read", spec[0], spec[1])
				}
			}

			// The HFS+ volume header must surface through every codec: it
			// lives inside the partition's first compressed chunk.
			var hdr [2]byte
			if _, err := d.ReadAt(hdr[:], hfsAtSector*Sector+1024); err != nil {
				t.Fatal(err)
			}
			if string(hdr[:]) != "H+" {
				t.Fatalf("no HFS+ volume header through the chunks: %q", hdr)
			}
		})
	}
}

// TestReadAtBounds pins the io.ReaderAt contract at the edges.
func TestReadAtBounds(t *testing.T) {
	d := openFixture(t, "tiny-UDZO.dmg")

	if n, err := d.ReadAt(make([]byte, 10), d.Size()+1); n != 0 || err != io.EOF {
		t.Fatalf("past the end: n=%d err=%v, want 0/io.EOF", n, err)
	}
	buf := make([]byte, 10)
	n, err := d.ReadAt(buf, d.Size()-4)
	if n != 4 || err != io.EOF {
		t.Fatalf("straddling the end: n=%d err=%v, want 4/io.EOF", n, err)
	}
	if !bytes.Equal(buf[:4], fullTail(t, d, 4)) {
		t.Fatal("straddling read returned wrong bytes")
	}
	if _, err := d.ReadAt(buf, -1); err == nil {
		t.Fatal("negative offset accepted")
	}
}

func fullTail(t *testing.T, d *Disk, n int) []byte {
	t.Helper()
	full, err := io.ReadAll(io.NewSectionReader(d, 0, d.Size()))
	if err != nil {
		t.Fatal(err)
	}
	return full[int64(len(full))-int64(n):]
}

// TestRejectsNonUDIF checks the trailer gate.
func TestRejectsNonUDIF(t *testing.T) {
	d, err := Open(bytes.NewReader(make([]byte, 4096)), 4096)
	if err == nil {
		t.Fatal("an all-zero file opened as a dmg")
	}
	_ = d
}

// TestChunkTableValidation checks the mish parser's invariants directly on
// hand-built tables (the plist stores tables base64-encoded, so corruption is
// easiest to exercise this way).
func TestChunkTableValidation(t *testing.T) {
	be := func(vals ...uint64) []byte {
		out := make([]byte, 0, len(vals)*8)
		var b [8]byte
		for _, v := range vals {
			binary.BigEndian.PutUint64(b[:], v)
			out = append(out, b[:]...)
		}
		return out
	}
	mish := func(declaredSectors uint64, entries ...[6]uint64) []byte {
		buf := []byte("mish")
		var v [8]byte
		binary.BigEndian.PutUint32(v[:], 4)
		buf = append(buf, v[:4]...) // version
		buf = append(buf, be(0)...) // first sector 0
		buf = append(buf, be(declaredSectors)...)
		buf = append(buf, be(0)...) // data offset
		var small [4]byte
		buf = append(buf, small[:]...)
		buf = append(buf, small[:]...)          // buffers needed, block descriptors
		buf = append(buf, make([]byte, 24)...)  // reserved
		buf = append(buf, make([]byte, 136)...) // checksum
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(entries))+1)
		buf = append(buf, n[:]...)
		for _, e := range entries {
			var t, c [4]byte
			binary.BigEndian.PutUint32(t[:], uint32(e[0]))
			buf = append(buf, t[:]...)
			buf = append(buf, c[:]...) // comment
			buf = append(buf, be(e[1], e[2], e[3], e[4])...)
		}
		var term, c2 [4]byte
		binary.BigEndian.PutUint32(term[:], 0xFFFFFFFF)
		buf = append(buf, term[:]...)
		buf = append(buf, c2[:]...)
		buf = append(buf, be(0, 0, 0, 0)...)
		return buf
	}

	// A consistent table: one raw chunk covering 2 sectors.
	if _, err := parseMish(mish(2, [6]uint64{1, 0, 2, 0, 1024}), 8192); err != nil {
		t.Fatalf("consistent table rejected: %v", err)
	}

	// The chunk sum must match the declared sector count.
	if _, err := parseMish(mish(3, [6]uint64{1, 0, 2, 0, 1024}), 8192); err == nil {
		t.Fatal("a table whose chunks sum to less than its declared sectors was accepted")
	}

	// Chunks must be contiguous and relative to the table start.
	if _, err := parseMish(mish(4, [6]uint64{1, 1, 2, 0, 1024}), 8192); err == nil {
		t.Fatal("a table whose first chunk skips a sector was accepted")
	}

	// The absolute sector range must fit the disk.
	if _, err := parseMish(mish(2, [6]uint64{1, 0, 2, 0, 1024}), 512); err == nil {
		t.Fatal("a table beyond the disk size was accepted")
	}

	// A negative/misplaced data offset is rejected before it can become a
	// wild file read.
	if _, err := parseMish(mish(2, [6]uint64{1, 0, 2, 1 << 63, 1024}), 8192); err == nil {
		t.Fatal("a chunk with a huge compressed offset was accepted")
	}
}

func hashMD5(b []byte) []byte {
	sum := md5.Sum(b)
	return sum[:]
}

// TestConcurrentReads exercises the reader from several goroutines at once,
// the shape extraction uses.
func TestConcurrentReads(t *testing.T) {
	d := openFixture(t, "tiny-UDZO.dmg")
	full, err := io.ReadAll(io.NewSectionReader(d, 0, d.Size()))
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			off := int64(w) * (d.Size() / workers)
			buf := make([]byte, 64<<10)
			for i := 0; i < 8; i++ {
				if _, err := d.ReadAt(buf, off+int64(i)*4096); err != nil {
					errs <- fmt.Errorf("worker %d: %w", w, err)
					return
				}
				if !bytes.Equal(buf, full[off+int64(i)*4096:off+int64(i)*4096+int64(len(buf))]) {
					errs <- fmt.Errorf("worker %d: wrong bytes", w)
					return
				}
			}
			errs <- nil
		}(w)
	}
	for w := 0; w < workers; w++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
