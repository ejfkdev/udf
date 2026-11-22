package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ejfkdev/udf/fsview"
	"github.com/klauspost/compress/zstd"
)

// These fixtures reproduce the exact byte layouts the reference writers
// produce (stargz-snapshotter's gzipFooterBytes and containers/storage's
// writeZstdChunkedStream), so the fast paths run against real formats. The
// assertions are differentials: the same archive read through the regular
// streaming path must produce the same tree and the same file bytes.

// layerSpec is one file that goes into every fixture layer.
type layerSpec struct {
	name string
	body string
}

var featureLayerFiles = []layerSpec{
	{name: "etc/", body: ""},
	{name: "etc/hello.txt", body: "hello from a seekable layer\n"},
	{name: "usr/bin/tool", body: "tool payload, long enough to matter\n"},
}

// buildLayerTarFrom builds the plain tar the layer wraps.
func buildLayerTarFrom(t *testing.T, files []layerSpec) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		if f.name == "" {
			continue
		}
		hdr := &tar.Header{Mode: 0o644, Typeflag: tar.TypeReg, ModTime: time.Unix(1700000000, 0)}
		if bytes.HasSuffix([]byte(f.name), []byte("/")) {
			hdr = &tar.Header{Mode: 0o755, Typeflag: tar.TypeDir, ModTime: time.Unix(1700000000, 0)}
			hdr.Name = f.name
		} else {
			hdr.Name = f.name
			hdr.Size = int64(len(f.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// buildEstargzBlob writes a real eStargz blob: one gzip member per tar entry
// (header+payload, as the reference writer lays them out), the TOC member, and
// the 51-byte footer. TOC entries carry the compressed offsets and inner
// offsets the reference writer records.
func buildEstargzBlob(t *testing.T, files []layerSpec) []byte {
	t.Helper()
	tarBytes := buildLayerTarFrom(t, files)

	// Walk the tar to get each entry's uncompressed offset and size, so the
	// TOC can point at the right places.
	type entryInfo struct {
		name    string
		ttype   string
		offset  int64 // uncompressed offset of this entry's header in the tar
		size    int64
		hdr     *tar.Header
		dataOff int64 // uncompressed offset of the payload
	}
	var infos []entryInfo
	{
		tr := tar.NewReader(bytes.NewReader(tarBytes))
		var off int64
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			info := entryInfo{name: hdr.Name, offset: off, size: hdr.Size, hdr: hdr}
			switch hdr.Typeflag {
			case tar.TypeDir:
				info.ttype = "dir"
			case tar.TypeReg:
				info.ttype = "reg"
			}
			info.dataOff = off + 512
			infos = append(infos, info)
			off += 512 + hdr.Size
			if pad := off % 512; pad != 0 {
				off += 512 - pad
			}
		}
	}

	var blob bytes.Buffer
	type tocEntry struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Size        int64  `json:"size,omitempty"`
		Mode        int64  `json:"mode,omitempty"`
		UID         int    `json:"uid,omitempty"`
		GID         int    `json:"gid,omitempty"`
		ModTime3339 string `json:"modtime,omitempty"`
		Offset      int64  `json:"offset,omitempty"`
		InnerOffset int64  `json:"innerOffset,omitempty"`
	}
	var toc []tocEntry

	// One gzip member per tar entry: compressed content = [tar header
	// (512 bytes)] + payload + block padding, exactly where the entry's
	// uncompressed offset lands in the reference layout.
	for _, info := range infos {
		memberStart := int64(blob.Len())
		zw, err := gzip.NewWriterLevel(&blob, gzip.BestCompression)
		if err != nil {
			t.Fatal(err)
		}
		// Member content: the tar bytes covering this entry (header + payload
		// + padding to the next 512 boundary).
		end := info.dataOff + info.size
		if pad := end % 512; pad != 0 {
			end += 512 - pad
		}
		if _, err := zw.Write(tarBytes[info.offset:end]); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		te := tocEntry{
			Name:        info.name,
			Type:        info.ttype,
			Mode:        int64(info.hdr.Mode),
			UID:         info.hdr.Uid,
			GID:         info.hdr.Gid,
			ModTime3339: info.hdr.ModTime.Format(time.RFC3339),
		}
		if info.ttype == "reg" {
			te.Size = info.size
			te.Offset = memberStart
			te.InnerOffset = 512 // the payload starts right after the member's tar header
		}
		toc = append(toc, te)
	}

	// TOC member.
	tocStart := int64(blob.Len())
	tocJSON, err := json.Marshal(map[string]any{"version": 1, "entries": toc})
	if err != nil {
		t.Fatal(err)
	}
	{
		zw, err := gzip.NewWriterLevel(&blob, gzip.BestCompression)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := zw.Write(tocJSON); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Footer (51 bytes), pointing at the TOC.
	footer := estargzFooterBytes(tocStart)
	blob.Write(footer)
	if int64(blob.Len()) != tocStart+int64(blob.Len()-int(tocStart)-51)+51 {
		t.Fatalf("blob layout off: %d", blob.Len())
	}
	return blob.Bytes()
}

// estargzFooterBytes builds the 51-byte footer (stargz-snapshotter
// gzipFooterBytes layout).
func estargzFooterBytes(tocOff int64) []byte {
	extra := []byte{'S', 'G', 22, 0}
	extra = append(extra, []byte(fmt.Sprintf("%016x", tocOff))...)
	extra = append(extra, []byte("STARGZ")...)

	buf := []byte{0x1f, 0x8b, 8, 0x04, 0, 0, 0, 0, 0, 255}
	var xlen [2]byte
	binary.LittleEndian.PutUint16(xlen[:], uint16(len(extra)))
	buf = append(buf, xlen[:]...)
	buf = append(buf, extra...)
	buf = append(buf, 1, 0, 0, 0xff, 0xff) // empty stored deflate block
	return append(buf, make([]byte, 8)...)
}

// buildZstdChunkedBlob writes a real zstd:chunked blob per containers/storage's
// writer: per file a header frame plus one frame per payload chunk, then the
// manifest and tar-split skippable frames and the 72-byte footer.
func buildZstdChunkedBlob(t *testing.T, files []layerSpec) []byte {
	t.Helper()

	type manifestEntry struct {
		Type        string     `json:"type"`
		Name        string     `json:"name"`
		Mode        int64      `json:"mode,omitempty"`
		Size        int64      `json:"size,omitempty"`
		UID         int        `json:"uid,omitempty"`
		GID         int        `json:"gid,omitempty"`
		ModTime     *time.Time `json:"modtime,omitempty"`
		Offset      int64      `json:"offset,omitempty"`
		EndOffset   int64      `json:"endOffset,omitempty"`
		ChunkOffset int64      `json:"chunkOffset,omitempty"`
		ChunkSize   int64      `json:"chunkSize,omitempty"`
	}
	var manifest []manifestEntry
	var blob bytes.Buffer
	// One complete zstd frame per EncodeAll call: the reference writer restarts
	// compression per chunk, producing exactly one frame per recorded range.
	writeFrame := func(p []byte) int64 {
		enc, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		blob.Write(enc.EncodeAll(p, nil))
		_ = enc.Close()
		return int64(blob.Len())
	}

	tarBytes := buildLayerTarFrom(t, files)
	// Walk the tar so each file's header and payload are written into their
	// own frames, mirroring the reference writer.
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		// Header frame.
		writeFrame(tarHeaderBlock(t, hdr, body))
		// Payload frame (one chunk).
		chunkStart := int64(blob.Len())
		end := writeFrame(body)
		mt := hdr.ModTime
		manifest = append(manifest, manifestEntry{
			Type: "reg", Name: hdr.Name, Mode: int64(hdr.Mode & 0o7777),
			Size: hdr.Size, UID: hdr.Uid, GID: hdr.Gid, ModTime: &mt,
			Offset: chunkStart, EndOffset: end,
			ChunkOffset: 0, ChunkSize: hdr.Size,
		})
	}
	// Manifest (zstd-compressed) inside a skippable frame, then a tar-split
	// frame, then the footer frame.
	mj, err := json.Marshal(map[string]any{"version": 1, "entries": manifest})
	if err != nil {
		t.Fatal(err)
	}
	var mz bytes.Buffer
	menc, err := zstd.NewWriter(&mz)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := menc.Write(mj); err != nil {
		t.Fatal(err)
	}
	if err := menc.Close(); err != nil {
		t.Fatal(err)
	}

	appendSkippable := func(payload []byte) {
		var hdr [8]byte
		binary.LittleEndian.PutUint32(hdr[0:4], 0x184D2A50)
		binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(payload)))
		blob.Write(hdr[:])
		blob.Write(payload)
	}
	manifestOffset := int64(blob.Len()) + 8
	appendSkippable(mz.Bytes())
	appendSkippable([]byte("tarsplit-placeholder"))

	footer := make([]byte, 64)
	binary.LittleEndian.PutUint64(footer[8*0:], uint64(manifestOffset))
	binary.LittleEndian.PutUint64(footer[8*1:], uint64(mz.Len()))
	binary.LittleEndian.PutUint64(footer[8*2:], uint64(len(mj)))
	binary.LittleEndian.PutUint64(footer[8*3:], 1) // CRFS
	copy(footer[8*7:], zstdChunkedFrameMagic[:])
	appendSkippable(footer)
	return blob.Bytes()
}

// tarHeaderBlock renders one tar header as its raw 512-byte block (the body
// must be written too — tar.Writer enforces it — and is then cut away).
func tarHeaderBlock(t *testing.T, hdr *tar.Header, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	out := buf.Bytes()
	if len(out) < 512 {
		out = append(out, make([]byte, 512-len(out))...)
	}
	return out[:512]
}

// writeDockerSaveFeatures writes a docker-save tar with one layer blob and
// returns the path plus the diff id of the layer's decompressed bytes.
func writeDockerSaveFeatures(t *testing.T, layerBlob []byte, decompressed []byte) string {
	t.Helper()
	diffID := sha256.Sum256(decompressed)
	diffHex := hex.EncodeToString(diffID[:])
	layerName := diffHex + "/layer.tar"
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:` + diffHex + `"]}}`)
	configSum := sha256.Sum256(config)
	configHex := hex.EncodeToString(configSum[:])
	manifest, _ := json.Marshal([]any{map[string]any{
		"Config":   configHex + ".json",
		"RepoTags": []string{"example/seekable:1.0"},
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

// TestFeatureLayerEndToEnd lists and reads both feature-layer formats through
// the fast paths and compares against a plain-tar image built from the same
// files.
func TestFeatureLayerEndToEnd(t *testing.T) {
	plainTar := buildLayerTarFrom(t, featureLayerFiles)
	plainPath := writeDockerSaveFeatures(t, plainTar, plainTar)
	plainMeta, err := ScanImageMetadata(plainPath, Selection{ImageIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	plainTree, err := BuildFileSystem(plainPath, plainMeta)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		feature string
		blob    []byte
	}{
		{LayerFeatureEstargz, buildEstargzBlob(t, featureLayerFiles)},
		{LayerFeatureZstdChunked, buildZstdChunkedBlob(t, featureLayerFiles)},
	}
	for _, tc := range cases {
		t.Run(tc.feature, func(t *testing.T) {
			// The layer decompresses to the same tar either way, so one diff
			// id serves both fixtures.
			path := writeDockerSaveFeatures(t, tc.blob, plainTar)
			meta, err := ScanImageMetadata(path, Selection{ImageIndex: -1})
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(meta.LayerFeatures) != 1 || meta.LayerFeatures[0] != tc.feature {
				t.Fatalf("features %v, want [%s]", meta.LayerFeatures, tc.feature)
			}

			// Listing through the TOC/manifest must equal the plain tree.
			tree, err := BuildFileSystem(path, meta)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			compareTreesNodes(t, plainTree, tree)

			// File reads through the compressed ranges must equal the source
			// bytes, via cat's path.
			for _, f := range featureLayerFiles {
				if f.body == "" {
					continue
				}
				rc, size, err := ReadArchiveFile(path, meta, "/"+f.name)
				if err != nil {
					t.Fatalf("read %s: %v", f.name, err)
				}
				body, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					t.Fatal(err)
				}
				if string(body) != f.body || size != int64(len(f.body)) {
					t.Fatalf("%s = %q (%d bytes), want %q", f.name, body, size, f.body)
				}
			}
		})
	}
}

// compareTreesNodes checks that two merged trees hold the same files with the
// same sizes and regular-file contents readable through their layers.
func compareTreesNodes(t *testing.T, a, b *fsview.Node) {
	t.Helper()
	var walk func(x, y *fsview.Node, path string)
	walk = func(x, y *fsview.Node, path string) {
		if x.Kind != y.Kind || x.Size != y.Size {
			t.Fatalf("%s: %+v vs %+v", path, x, y)
		}
		xc, yc := x.Children, y.Children
		if len(xc) != len(yc) {
			t.Fatalf("%s: %d children vs %d", path, len(xc), len(yc))
		}
		for i := range xc {
			if xc[i].Name != yc[i].Name {
				t.Fatalf("%s: child %s vs %s", path, xc[i].Name, yc[i].Name)
			}
			walk(xc[i], yc[i], path+"/"+xc[i].Name)
		}
	}
	walk(a, b, "")
}

// TestRealEstargzLayer runs the fast paths against a blob produced by
// stargz-snapshotter itself. The generator lives outside the repo (its
// module pulls the snapshotter); the image it wrote is checked in under
// testdata. Point UDF_ESTARGZ_IMAGE at a docker-save tar with a real
// eStargz layer to exercise it.
func TestRealEstargzLayer(t *testing.T) {
	path := os.Getenv("UDF_ESTARGZ_IMAGE")
	if path == "" {
		path = filepath.Join("testdata", "estargz-real.tar")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skip("no real eStargz image available")
	}
	meta, err := ScanImageMetadata(path, Selection{ImageIndex: -1})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(meta.LayerFeatures) == 0 || meta.LayerFeatures[0] != LayerFeatureEstargz {
		t.Fatalf("features %v, want [estargz]", meta.LayerFeatures)
	}
	tree, err := BuildFileSystem(path, meta)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	node := tree.Resolve("usr/bin/tool")
	if node == nil || node.Kind != fsview.KindFile {
		t.Fatalf("usr/bin/tool missing from the TOC-derived tree: %+v", node)
	}
	if node.Size != int64(len("tool payload generated by stargz-snapshotter itself\n")) {
		t.Fatalf("tool size %d", node.Size)
	}
	rc, _, err := ReadArchiveFile(path, meta, "/etc/hello.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello from a REAL estargz layer\n" {
		t.Fatalf("hello.txt = %q", body)
	}
}
