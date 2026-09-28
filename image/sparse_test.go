package image

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// writeSparseImage wraps raw in an Android sparse image, using one chunk per
// run of equal 512-byte sectors so the fixture exercises every chunk kind: raw
// data, fill (a repeated word) and don't care (zeros), followed by a crc32
// metadata chunk. It mirrors what libsparse produces for a mostly-empty image.
func writeSparseImage(t *testing.T, raw []byte, blkSz int) string {
	t.Helper()

	const maxRunBlocks = 8
	if len(raw)%blkSz != 0 {
		t.Fatalf("raw image is not block aligned")
	}
	blocks := len(raw) / blkSz
	kindOf := func(i int) (uint16, uint32) {
		block := raw[i*blkSz : (i+1)*blkSz]
		zero := true
		for _, b := range block {
			if b != 0 {
				zero = false
				break
			}
		}
		if zero {
			return sparseChunkDontCa, 0
		}
		word := binary.LittleEndian.Uint32(block[:4])
		for j := 0; j < len(block); j += 4 {
			if binary.LittleEndian.Uint32(block[j:j+4]) != word {
				return sparseChunkRaw, 0
			}
		}
		return sparseChunkFill, word
	}

	type chunk struct {
		kind uint16
		body []byte
		blks int64
		fill uint32
	}
	var chunks []chunk
	for i := 0; i < blocks; {
		kind, word := kindOf(i)
		j := i + 1
		for j < blocks && (j-i) < maxRunBlocks {
			k, w := kindOf(j)
			if k != kind || w != word {
				break
			}
			j++
		}
		c := chunk{kind: kind, blks: int64(j - i), fill: word}
		switch kind {
		case sparseChunkRaw:
			c.body = raw[i*blkSz : j*blkSz]
		case sparseChunkFill:
			c.body = []byte{byte(word), byte(word >> 8), byte(word >> 16), byte(word >> 24)}
		}
		chunks = append(chunks, c)
		i = j
	}

	var buf bytes.Buffer
	hdr := make([]byte, sparseFileHdrSz)
	copy(hdr, sparseMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], 1) // major
	binary.LittleEndian.PutUint16(hdr[8:10], sparseFileHdrSz)
	binary.LittleEndian.PutUint16(hdr[10:12], sparseChunkHdr)
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(blkSz))
	binary.LittleEndian.PutUint32(hdr[16:20], uint32(len(raw)/blkSz))
	binary.LittleEndian.PutUint32(hdr[20:24], uint32(len(chunks)+1))
	buf.Write(hdr)

	for _, c := range chunks {
		ch := make([]byte, sparseChunkHdr)
		binary.LittleEndian.PutUint16(ch[0:2], c.kind)
		binary.LittleEndian.PutUint32(ch[4:8], uint32(c.blks))
		binary.LittleEndian.PutUint32(ch[8:12], uint32(sparseChunkHdr+len(c.body)))
		buf.Write(ch)
		buf.Write(c.body)
	}
	// Trailing crc32 metadata chunk: contributes nothing to the image.
	ch := make([]byte, sparseChunkHdr)
	binary.LittleEndian.PutUint16(ch[0:2], sparseChunkCRC32)
	binary.LittleEndian.PutUint32(ch[4:8], 4)
	binary.LittleEndian.PutUint32(ch[8:12], sparseChunkHdr+4)
	buf.Write(ch)
	buf.Write(make([]byte, 4))

	path := filepath.Join(t.TempDir(), "system.img")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSparseReaderExpandsChunks checks that every chunk kind expands to the
// bytes of the original raw image, read both whole and in scattered ranges.
func TestSparseReaderExpandsChunks(t *testing.T) {
	raw := make([]byte, 64*1024)
	copy(raw[0*512:], bytes.Repeat([]byte{0xAA, 0xBB, 0xCC, 0xDD}, 128)) // fill-able
	copy(raw[8*512:], []byte("raw data starts here"))                    // raw
	copy(raw[16*512:], bytes.Repeat([]byte{0x11}, 512))                  // fill
	// The rest stays zero: don't care.

	path := writeSparseImage(t, raw, 4096)
	r, err := openSparse(path)
	if err != nil {
		t.Fatalf("open sparse: %v", err)
	}
	defer r.Close()
	if r.Size() != int64(len(raw)) {
		t.Fatalf("size = %d, want %d", r.Size(), len(raw))
	}

	got := make([]byte, len(raw))
	if _, err := r.ReadAt(got, 0); err != nil {
		t.Fatalf("read all: %v", err)
	}
	if !bytes.Equal(got, raw) {
		for i := range got {
			if got[i] != raw[i] {
				t.Fatalf("expanded image differs at offset %d: %#x vs %#x", i, got[i], raw[i])
			}
		}
	}

	for _, tc := range []struct{ off, n int }{
		{0, 4}, {100, 300}, {7 * 512, 600}, {16 * 512, 512}, {30 * 512, 1000}, {len(raw) - 10, 10},
	} {
		part := make([]byte, tc.n)
		read, err := r.ReadAt(part, int64(tc.off))
		if err != nil && read != tc.n {
			t.Fatalf("read [%d,+%d): %v", tc.off, tc.n, err)
		}
		if !bytes.Equal(part, raw[tc.off:tc.off+tc.n]) {
			t.Fatalf("range [%d,+%d) differs", tc.off, tc.n)
		}
	}

	// Reading past the end reports EOF after the available bytes.
	tail := make([]byte, 32)
	n, err := r.ReadAt(tail, int64(len(raw))-8)
	if n != 8 || err != io.EOF {
		t.Fatalf("tail read = %d, %v; want 8, EOF", n, err)
	}
}

// TestSparsePartitionedExt4Image runs the whole disk pipeline on a sparse
// container: the same partitioned ext4 image the qcow2 and QED tests use, so
// the sparse layer is exercised end to end (metadata, listing, extraction).
func TestSparsePartitionedExt4Image(t *testing.T) {
	rawPath := createPartitionedExt4Image(t)
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	sparsePath := writeSparseImage(t, raw, 4096)

	meta, err := ScanDiskMetadata(sparsePath)
	if err != nil {
		t.Fatalf("scan sparse metadata: %v", err)
	}
	if len(meta.Disks) != 1 || meta.Disks[0].Format != "sparse" ||
		meta.Disks[0].Volume != "p1" || meta.Disks[0].Filesystem != "ext4" {
		t.Fatalf("unexpected disk metadata: %+v", meta)
	}

	out := filepath.Join(t.TempDir(), "rootfs")
	if _, err := ExtractDiskVolumes(sparsePath, out, 1<<16); err != nil {
		t.Fatalf("extract sparse volumes: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(out, "etc", "passwd")); err != nil || string(data) != "root:x:0:0\n" {
		t.Fatalf("unexpected extracted passwd: %q err=%v", data, err)
	}
}
