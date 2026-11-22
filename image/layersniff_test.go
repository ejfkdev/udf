package image

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// The two fixture builders reproduce the exact trailer bytes the reference
// implementations write, so the sniffer is checked against real formats, not
// just its own constants.

// appendEstargzFooter appends the 51-byte eStargz footer, byte-for-byte the
// way stargz-snapshotter's gzipFooterBytes builds it: a gzip member with an
// SG extra field (TOC offset + STARGZ magic), an empty stored deflate block
// and a zero CRC/size.
func appendEstargzFooter(t *testing.T, blob []byte) []byte {
	t.Helper()
	extra := []byte{'S', 'G', 22, 0} // SI1, SI2, subfield length (LE), per RFC 1952
	extra = append(extra, []byte("0000000000000400")...)
	extra = append(extra, []byte("STARGZ")...)

	buf := []byte{0x1f, 0x8b, 8, 0x04, 0, 0, 0, 0, 0, 255} // gzip header, FEXTRA
	var xlen [2]byte
	binary.LittleEndian.PutUint16(xlen[:], uint16(len(extra)))
	buf = append(buf, xlen[:]...)
	buf = append(buf, extra...)
	// Empty stored deflate block, then a zero CRC32/ISIZE.
	buf = append(buf, 1, 0, 0, 0xff, 0xff)
	buf = append(buf, make([]byte, 8)...)
	if len(buf) != estargzFooterSize {
		t.Fatalf("footer is %d bytes, want %d", len(buf), estargzFooterSize)
	}
	return append(append([]byte{}, blob...), buf...)
}

// appendZstdChunkedTail appends the zstd:chunked trailer: one skippable frame
// holding the 64-byte footer, whose last 8 bytes are the frame magic.
func appendZstdChunkedTail(t *testing.T, blob []byte) []byte {
	t.Helper()
	footer := make([]byte, 64)
	binary.LittleEndian.PutUint64(footer[8*0:], uint64(len(blob)))      // manifest offset
	binary.LittleEndian.PutUint64(footer[8*1:], 128)                    // manifest length
	binary.LittleEndian.PutUint64(footer[8*2:], 4096)                   // manifest uncompressed
	binary.LittleEndian.PutUint64(footer[8*3:], 1)                      // manifest type (CRFS)
	binary.LittleEndian.PutUint64(footer[8*4:], uint64(len(blob))+1024) // tar-split offset
	binary.LittleEndian.PutUint64(footer[8*5:], 64)                     // tar-split compressed
	binary.LittleEndian.PutUint64(footer[8*6:], 2048)                   // tar-split uncompressed
	copy(footer[8*7:], zstdChunkedFrameMagic[:])

	out := append([]byte{}, blob...)
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], 0x184D2A50)
	binary.LittleEndian.PutUint32(hdr[4:8], 64)
	out = append(out, hdr[:]...)
	return append(out, footer...)
}

func TestSniffLayerTail(t *testing.T) {
	blob := bytes.Repeat([]byte("payload "), 4096)

	// Plain blobs are not misdetected.
	if got := sniffLayerTail(blob[len(blob)-51:]); got != "" {
		t.Fatalf("plain blob sniffed as %q", got)
	}
	if got := sniffLayerTail(nil); got != "" {
		t.Fatalf("empty tail sniffed as %q", got)
	}

	estargz := appendEstargzFooter(t, blob)
	if got := sniffLayerTail(estargz[len(estargz)-estargzFooterSize:]); got != LayerFeatureEstargz {
		t.Fatalf("estargz tail sniffed as %q", got)
	}

	chunked := appendZstdChunkedTail(t, blob)
	if got := sniffLayerTail(chunked[len(chunked)-zstdChunkedTailSize:]); got != LayerFeatureZstdChunked {
		t.Fatalf("zstd-chunked tail sniffed as %q", got)
	}

	// Corrupting one magic byte must clear the detection.
	broken := append([]byte{}, chunked[len(chunked)-zstdChunkedTailSize:]...)
	broken[len(broken)-1] ^= 0xFF
	if got := sniffLayerTail(broken); got != "" {
		t.Fatalf("corrupted tail sniffed as %q", got)
	}
}

// TestSniffEndToEnd builds docker-save archives whose layers carry each
// feature and checks that the metadata scan reports them.
func TestSniffEndToEnd(t *testing.T) {
	tarBody := smallLayerTar(t)

	// eStargz layer stored as plain gzip-framed bytes: the footer member rides
	// after the payload.
	estargzPath := writeDockerSaveLayer(t, appendEstargzFooter(t, tarBody), tarBody)
	meta, err := ScanImageMetadata(estargzPath, Selection{ImageIndex: -1})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(meta.LayerFeatures) != 1 || meta.LayerFeatures[0] != LayerFeatureEstargz {
		t.Fatalf("features %v, want [%s]", meta.LayerFeatures, LayerFeatureEstargz)
	}

	// A zstd layer with a chunked trailer; the decompressed bytes are exactly
	// the tar.
	zw, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	blob := zw.EncodeAll(tarBody, nil)
	chunkedPath := writeDockerSaveLayer(t, appendZstdChunkedTail(t, blob), tarBody)
	meta, err = ScanImageMetadata(chunkedPath, Selection{ImageIndex: -1})
	if err != nil {
		t.Fatalf("scan zstd: %v", err)
	}
	if len(meta.LayerFeatures) != 1 || meta.LayerFeatures[0] != LayerFeatureZstdChunked {
		t.Fatalf("features %v, want [%s]", meta.LayerFeatures, LayerFeatureZstdChunked)
	}
}

// TestSniffOCILayoutDirectory covers the directory-layout read path: layer
// blobs are files, so the tail is a direct read.
func TestSniffOCILayoutDirectory(t *testing.T) {
	tarBody := smallLayerTar(t)
	blob := appendEstargzFooter(t, tarBody)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	layerName := "blobs/sha256/aa11"
	if err := os.WriteFile(filepath.Join(dir, layerName), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sniffLayerFeatures(dir, []string{layerName}); got[0] != LayerFeatureEstargz {
		t.Fatalf("oci dir features %v", got)
	}

	// A layer without feature metadata stays empty.
	plain := filepath.Join(dir, "blobs", "sha256", "bb22")
	if err := os.WriteFile(plain, tarBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sniffLayerFeatures(dir, []string{"blobs/sha256/bb22"}); got[0] != "" {
		t.Fatalf("plain layer sniffed as %q", got[0])
	}
}

// smallLayerTar builds a small tar with one file.
func smallLayerTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "etc/hello.txt", Mode: 0o644, Size: int64(len("hello\n")), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeDockerSaveLayer writes a docker-save tar whose single layer member
// holds layerBlob; diffID is computed over wantDecompressed, the bytes the
// layer decompresses to.
func writeDockerSaveLayer(t *testing.T, layerBlob, wantDecompressed []byte) string {
	t.Helper()
	diffID := sha256.Sum256(wantDecompressed)
	diffHex := hex.EncodeToString(diffID[:])
	layerName := diffHex + "/layer.tar"
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:` + diffHex + `"]}}`)
	configSum := sha256.Sum256(config)
	configHex := hex.EncodeToString(configSum[:])
	manifest, _ := json.Marshal([]any{map[string]any{
		"Config":   configHex + ".json",
		"RepoTags": []string{"example/app:1.0"},
		"Layers":   []string{layerName},
	}})

	path := filepath.Join(t.TempDir(), "image.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	write := func(name string, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", manifest)
	write(configHex+".json", config)
	write(layerName, layerBlob)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
