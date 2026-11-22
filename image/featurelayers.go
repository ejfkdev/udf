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
	"strconv"
	"strings"
	"time"

	"github.com/ejfkdev/udf/fsutil"
	"github.com/ejfkdev/udf/fsview"
	"github.com/ejfkdev/udf/types"
	"github.com/klauspost/compress/zstd"
)

// Seekable-compression layer fast paths: eStargz and zstd:chunked layers carry
// a table of contents in their compressed bytes, so the directory comes from
// metadata alone (no payload decompression) and one file costs reading only
// the compressed ranges that hold it. Both readers here are independent
// implementations of the published formats: eStargz per stargz-snapshotter
// (estargz/gzip.go, types.go) and zstd:chunked per containers/storage
// (pkg/chunked), whose byte layouts were verified against those sources.

// featureEntry is one layer entry as the format's table records it.
type featureEntry struct {
	hdr *tar.Header

	// estargz: the compressed member range and the payload's offset inside the
	// decompressed member.
	compOff, compLen int64
	innerOff         int64

	// zstd:chunked: one compressed frame range per payload chunk, in chunk
	// order. Each range decodes to pure chunk bytes (the tar header lives in
	// its own frame outside these ranges).
	chunks []featureChunk
}

type featureChunk struct {
	off, len int64
}

// featureLayerIndex is a parsed layer table plus access to the layer blob.
type featureLayerIndex struct {
	feature string
	entries []featureEntry
	byName  map[string][]*featureEntry // data chunks grouped per tar entry
}

// layerBlob is random access to one layer's compressed bytes, however the
// archive stores them: a plain file (OCI layout directory), a byte range of a
// plain tar (docker save), or a member reached through an already-built outer
// index.
type layerBlob struct {
	size   int64
	readAt func(off int64, n int) ([]byte, error)
	close  func() error
}

// layerBlobAccess opens one layer blob. When buildIndex is true an outer
// tar.gz/tar.zst index may be built for it (worth it for a real read); the
// sniffer passes false so a metadata-only peek never triggers a build.
func layerBlobAccess(path, layerName string, buildIndex bool) (*layerBlob, error) {
	if b, err := layerBlobFile(path, layerName); err == nil {
		return b, nil
	}
	if b, err := layerBlobTarMember(path, layerName); err == nil {
		return b, nil
	}
	if buildIndex {
		if b, err := layerBlobIndexed(path, layerName); err == nil {
			return b, nil
		}
	} else if b, err := layerBlobCachedIndex(path, layerName); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("layer %s has no random-access path", layerName)
}

// layerBlobFile covers OCI layout directories: blobs are plain files.
func layerBlobFile(path, layerName string) (*layerBlob, error) {
	full := filepath.Join(path, layerName)
	st, err := os.Stat(full)
	if err != nil || st.IsDir() {
		return nil, fmt.Errorf("not a file")
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	return &layerBlob{
		size: st.Size(),
		readAt: func(off int64, n int) ([]byte, error) {
			buf := make([]byte, n)
			if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
				return nil, err
			}
			return buf, nil
		},
		close: func() error { return f.Close() },
	}, nil
}

// layerBlobTarMember covers uncompressed docker-save tars: the member is a
// byte range located by walking the header chain.
func layerBlobTarMember(path, layerName string) (*layerBlob, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	start, size, ok := locateTarMember(f, st.Size(), layerName)
	if !ok {
		_ = f.Close()
		return nil, fmt.Errorf("member %s not found", layerName)
	}
	sr := io.NewSectionReader(f, start, size)
	return &layerBlob{
		size: size,
		readAt: func(off int64, n int) ([]byte, error) {
			buf := make([]byte, n)
			if _, err := sr.ReadAt(buf, off); err != nil && err != io.EOF {
				return nil, err
			}
			return buf, nil
		},
		close: func() error { return f.Close() },
	}, nil
}

// layerBlobIndexed reads through the outer archive's index, building it when
// missing (a real read justifies the one pass). Each read is a positioned
// sequential read from the index.
func layerBlobIndexed(path, layerName string) (*layerBlob, error) {
	ix, ok := ensureImageIndex(path)
	if !ok {
		return nil, fmt.Errorf("no index for %s", path)
	}
	e, ok := ix.Lookup(layerName)
	if !ok {
		_ = ix.Close()
		return nil, fmt.Errorf("layer %s not in index", layerName)
	}
	read := func(off int64, n int) ([]byte, error) {
		rc, err := ix.ReadAt(e.OutOff + off)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		buf := make([]byte, n)
		if _, err := io.ReadFull(rc, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	return &layerBlob{
		size:   e.Size,
		readAt: read,
		close:  func() error { return ix.Close() },
	}, nil
}

// layerBlobCachedIndex is layerBlobIndexed without ever building the index.
func layerBlobCachedIndex(path, layerName string) (*layerBlob, error) {
	ix, ok := loadImageIndex(path)
	if !ok {
		return nil, fmt.Errorf("no cached index for %s", path)
	}
	e, ok := ix.Lookup(layerName)
	if !ok {
		_ = ix.Close()
		return nil, fmt.Errorf("layer %s not in index", layerName)
	}
	read := func(off int64, n int) ([]byte, error) {
		rc, err := ix.ReadAt(e.OutOff + off)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		buf := make([]byte, n)
		if _, err := io.ReadFull(rc, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	return &layerBlob{
		size:   e.Size,
		readAt: read,
		close:  func() error { return ix.Close() },
	}, nil
}

// locateTarMembers walks an uncompressed tar's header chain once (validating
// each header's checksum, so compressed files never masquerade as tar) and
// returns every member body's absolute offset and size.
func locateTarMembers(f *os.File, fileSize int64) (map[string]tarMember, bool) {
	out := map[string]tarMember{}
	var hdr [512]byte
	pos := int64(0)
	for pos+512 <= fileSize {
		if _, err := f.ReadAt(hdr[:], pos); err != nil {
			return out, false
		}
		if isZeroBlock(hdr[:]) || !validTarHeader(hdr[:]) {
			// End of archive, or not a tar at all (the walk stops rather than
			// parsing compressed bytes as headers).
			return out, true
		}
		size, err := strconv.ParseInt(string(bytes.TrimRight(hdr[124:136], " \x00")), 8, 64)
		if err != nil || size < 0 {
			return out, false
		}
		name := tarHeaderName(hdr[:])
		bodyStart := pos + 512
		if _, dup := out[name]; !dup && bodyStart+size <= fileSize {
			out[name] = tarMember{start: bodyStart, size: size}
		}
		pos = bodyStart + size
		if pos%512 != 0 {
			pos += 512 - pos%512
		}
	}
	return out, true
}

// tarMember is one member's body location in a plain tar file.
type tarMember struct {
	start, size int64
}

// locateTarMember finds one member via the single-walk locator.
func locateTarMember(f *os.File, fileSize int64, name string) (int64, int64, bool) {
	members, ok := locateTarMembers(f, fileSize)
	if !ok {
		return 0, 0, false
	}
	m, ok := members[name]
	return m.start, m.size, ok
}

// validTarHeader checks the tar header's checksum field.
func validTarHeader(h []byte) bool {
	stored, err := strconv.ParseUint(strings.TrimRight(string(h[148:156]), " \x00"), 8, 32)
	if err != nil {
		return false
	}
	sum := 0
	for i, c := range h {
		if i >= 148 && i < 156 {
			sum += int(' ')
		} else {
			sum += int(c)
		}
	}
	return int(stored) == sum
}

func tarHeaderName(h []byte) string {
	if i := bytes.IndexByte(h[:100], 0); i >= 0 {
		return string(h[:i])
	}
	return string(h[:100])
}

// ---------- eStargz ----------

// parseEstargzTOC reads the 51-byte footer, then the TOC member it points at,
// and builds the entry table. Nothing else of the blob is touched.
func parseEstargzTOC(blob *layerBlob) (*featureLayerIndex, error) {
	if blob.size < estargzFooterSize {
		return nil, fmt.Errorf("blob too small for an eStargz footer")
	}
	tail, err := blob.readAt(blob.size-estargzFooterSize, estargzFooterSize)
	if err != nil {
		return nil, err
	}
	if !isEstargzFooter(tail) {
		return nil, fmt.Errorf("not an eStargz footer")
	}
	// Re-parse the extra field for the TOC offset (the check already vetted
	// the shape).
	zr, err := gzip.NewReader(bytes.NewReader(tail))
	if err != nil {
		return nil, err
	}
	extra := zr.Header.Extra
	_ = zr.Close()
	tocOffset, err := strconv.ParseInt(string(extra[4:20]), 16, 64)
	if err != nil || tocOffset < 0 || tocOffset > blob.size-estargzFooterSize {
		return nil, fmt.Errorf("bad eStargz TOC offset %d", tocOffset)
	}
	tocLen := blob.size - estargzFooterSize - tocOffset
	tocBytes, err := blob.readAt(tocOffset, int(tocLen))
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(tocBytes))
	if err != nil {
		return nil, fmt.Errorf("toc member: %w", err)
	}
	defer gz.Close()
	var toc struct {
		Version int `json:"version"`
		Entries []struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			Size        int64  `json:"size"`
			Mode        int64  `json:"mode"`
			UID         int    `json:"uid"`
			GID         int    `json:"gid"`
			ModTime3339 string `json:"modtime"`
			LinkName    string `json:"linkName"`
			Offset      int64  `json:"offset"`
			InnerOffset int64  `json:"innerOffset"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(gz).Decode(&toc); err != nil {
		return nil, fmt.Errorf("toc json: %w", err)
	}

	ix := &featureLayerIndex{feature: LayerFeatureEstargz, byName: map[string][]*featureEntry{}}
	// nextOffset[i]: the compressed offset of the next member boundary after
	// entries[i].Offset — the member holding entry i ends there.
	for i, e := range toc.Entries {
		if e.Type == "chunk" {
			continue // payload continuation of the previous reg entry
		}
		next := tocOffset
		for j := i + 1; j < len(toc.Entries); j++ {
			if toc.Entries[j].Offset > 0 {
				next = toc.Entries[j].Offset
				break
			}
		}
		if e.Offset > 0 && next <= e.Offset {
			next = tocOffset
		}
		hdr := &tar.Header{
			Name:     e.Name,
			Mode:     toFileMode(e.Type, e.Mode),
			Size:     e.Size,
			Uid:      e.UID,
			Gid:      e.GID,
			Linkname: e.LinkName,
			ModTime:  parseRFC3339(e.ModTime3339),
		}
		hdr.Typeflag = estargzTypeflag(e.Type)
		fe := &featureEntry{
			hdr:      hdr,
			compOff:  e.Offset,
			compLen:  next - e.Offset,
			innerOff: e.InnerOffset,
		}
		if e.Offset <= 0 && e.Type == "reg" && e.Size > 0 {
			// Data-less record (an empty member marker): keep the header but
			// mark the range empty.
			fe.compLen = 0
		}
		ix.entries = append(ix.entries, *fe)
	}
	for i := range ix.entries {
		ix.byName[ix.entries[i].hdr.Name] = append(ix.byName[ix.entries[i].hdr.Name], &ix.entries[i])
	}
	return ix, nil
}

// readEstargzEntry returns one entry's payload, reading and decompressing only
// the member range that holds it.
func readEstargzEntry(blob *layerBlob, ix *featureLayerIndex, e *featureEntry) ([]byte, error) {
	if e.compLen <= 0 {
		return []byte{}, nil
	}
	data, err := blob.readAt(e.compOff, int(e.compLen))
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	if e.innerOff+e.hdr.Size > int64(len(out)) {
		return nil, fmt.Errorf("entry %s: payload outside its member", e.hdr.Name)
	}
	return out[e.innerOff : e.innerOff+e.hdr.Size], nil
}

func estargzTypeflag(t string) byte {
	switch t {
	case "dir":
		return tar.TypeDir
	case "reg":
		return tar.TypeReg
	case "symlink":
		return tar.TypeSymlink
	case "hardlink":
		return tar.TypeLink
	case "char":
		return tar.TypeChar
	case "block":
		return tar.TypeBlock
	case "fifo":
		return tar.TypeFifo
	default:
		return tar.TypeReg
	}
}

// ---------- zstd:chunked ----------

var zstdChunkedManifestTypeCRFS = uint64(1)

// parseZstdChunkedManifest reads the trailing skippable frame, then the
// manifest it points at, and builds the entry table.
func parseZstdChunkedManifest(blob *layerBlob) (*featureLayerIndex, error) {
	if blob.size < zstdChunkedTailSize {
		return nil, fmt.Errorf("blob too small for a zstd:chunked footer")
	}
	tail, err := blob.readAt(blob.size-zstdChunkedTailSize, zstdChunkedTailSize)
	if err != nil {
		return nil, err
	}
	if !isZstdChunkedTail(tail) {
		return nil, fmt.Errorf("not a zstd:chunked footer")
	}
	footer := tail[8:] // skip the skippable frame header
	manifestOffset := int64(binary.LittleEndian.Uint64(footer[8*0:]))
	manifestLen := int64(binary.LittleEndian.Uint64(footer[8*1:]))
	manifestUncompressed := int64(binary.LittleEndian.Uint64(footer[8*2:]))
	manifestType := binary.LittleEndian.Uint64(footer[8*3:])
	if manifestType != zstdChunkedManifestTypeCRFS {
		return nil, fmt.Errorf("unsupported zstd:chunked manifest type %d", manifestType)
	}
	if manifestOffset < 0 || manifestLen <= 0 || manifestOffset+manifestLen > blob.size-zstdChunkedTailSize {
		return nil, fmt.Errorf("manifest extent [%d, %d) outside the blob", manifestOffset, manifestOffset+manifestLen)
	}
	compressed, err := blob.readAt(manifestOffset, int(manifestLen))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	manifestBytes, err := dec.DecodeAll(compressed, make([]byte, 0, manifestUncompressed))
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var manifest struct {
		Version int `json:"version"`
		Entries []struct {
			Type        string     `json:"type"`
			Name        string     `json:"name"`
			Linkname    string     `json:"linkName"`
			Mode        int64      `json:"mode"`
			Size        int64      `json:"size"`
			UID         int        `json:"uid"`
			GID         int        `json:"gid"`
			ModTime     *time.Time `json:"modtime"`
			Offset      int64      `json:"offset"`
			EndOffset   int64      `json:"endOffset"`
			ChunkOffset int64      `json:"chunkOffset"`
			ChunkSize   int64      `json:"chunkSize"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("manifest json: %w", err)
	}

	ix := &featureLayerIndex{feature: LayerFeatureZstdChunked, byName: map[string][]*featureEntry{}}
	byName := map[string]*featureEntry{}
	var order []string
	for _, e := range manifest.Entries {
		if e.Offset == 0 && e.EndOffset == 0 && e.Type == "chunk" {
			continue // continuation without a range (never happens per the writer, but stay safe)
		}
		fe, ok := byName[e.Name]
		if !ok {
			var mtime time.Time
			if e.ModTime != nil {
				mtime = *e.ModTime
			}
			fe = &featureEntry{hdr: &tar.Header{
				Name:     e.Name,
				Mode:     toFileMode(e.Type, e.Mode),
				Size:     e.Size,
				Uid:      e.UID,
				Gid:      e.GID,
				Linkname: e.Linkname,
				ModTime:  mtime,
				Typeflag: estargzTypeflag(e.Type),
			}}
			byName[e.Name] = fe
			order = append(order, e.Name)
		}
		if e.EndOffset > e.Offset {
			fe.chunks = append(fe.chunks, featureChunk{off: e.Offset, len: e.EndOffset - e.Offset})
		}
	}
	// Flatten in first-seen order; byName points at the stored elements so the
	// chunk lists travel with them.
	for _, name := range order {
		fe := byName[name]
		ix.entries = append(ix.entries, *fe)
		ix.byName[name] = []*featureEntry{&ix.entries[len(ix.entries)-1]}
	}
	return ix, nil
}

// readZstdChunkedEntry returns one entry's payload by decoding exactly the
// frames that hold its chunks.
func readZstdChunkedEntry(blob *layerBlob, ix *featureLayerIndex, e *featureEntry) ([]byte, error) {
	if len(e.chunks) == 0 {
		return []byte{}, nil
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	var out []byte
	for _, c := range e.chunks {
		data, err := blob.readAt(c.off, int(c.len))
		if err != nil {
			return nil, err
		}
		decoded, err := dec.DecodeAll(data, make([]byte, 0, 32<<10))
		if err != nil {
			return nil, fmt.Errorf("entry %s chunk at %d: %w", e.hdr.Name, c.off, err)
		}
		out = append(out, decoded...)
	}
	if int64(len(out)) < e.hdr.Size {
		return nil, fmt.Errorf("entry %s decoded %d bytes, want %d", e.hdr.Name, len(out), e.hdr.Size)
	}
	return out[:e.hdr.Size], nil
}

// ---------- shared plumbing ----------

func toFileMode(kind string, mode int64) int64 {
	m := mode & 0o7777
	switch kind {
	case "dir":
		m |= 0o40000
	case "symlink":
		m |= 0o120000
	case "char":
		m |= 0o020000
	case "block":
		m |= 0o060000
	case "fifo":
		m |= 0o010000
	}
	return m
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// parseFeatureLayer builds the table for one layer, or nil when the blob is
// unreachable or the feature does not parse.
func parseFeatureLayer(path, layerName, feature string, buildIndex bool) *featureLayerIndex {
	blob, err := layerBlobAccess(path, layerName, buildIndex)
	if err != nil {
		return nil
	}
	defer blob.close()
	switch feature {
	case LayerFeatureEstargz:
		ix, err := parseEstargzTOC(blob)
		if err != nil {
			return nil
		}
		return ix
	case LayerFeatureZstdChunked:
		ix, err := parseZstdChunkedManifest(blob)
		if err != nil {
			return nil
		}
		return ix
	}
	return nil
}

// featureLayerDirectories parses the tables of every layer into a merged tree.
// It reports nil unless EVERY layer carries a feature and every table parses —
// a mixed image falls back to the streaming paths.
func featureLayerDirectories(imageTarPath string, meta *types.ImageMetadata) (*fsview.Node, bool) {
	if len(meta.LayerOrder) == 0 || len(meta.LayerFeatures) != len(meta.LayerOrder) {
		return nil, false
	}
	layers := make(map[string]*fsview.ParsedLayer, len(meta.LayerOrder))
	for i, name := range meta.LayerOrder {
		if meta.LayerFeatures[i] == "" {
			return nil, false
		}
		ix := parseFeatureLayer(imageTarPath, name, meta.LayerFeatures[i], false)
		if ix == nil {
			return nil, false
		}
		headers := make([]*tar.Header, 0, len(ix.entries))
		for j := range ix.entries {
			headers = append(headers, ix.entries[j].hdr)
		}
		layers[name] = fsview.NewParsedLayer(headers)
	}
	tree, err := fsview.BuildParsed(meta.LayerOrder, layers)
	if err != nil {
		return nil, false
	}
	return tree, true
}

// openFeatureLayerFile reads one file out of a feature layer: only the
// compressed ranges holding its payload are fetched and decoded.
func openFeatureLayerFile(imageTarPath string, meta *types.ImageMetadata, layerName, entryName string) (io.ReadCloser, int64, error) {
	idx := -1
	for i, n := range meta.LayerOrder {
		if n == layerName {
			idx = i
			break
		}
	}
	if idx < 0 || idx >= len(meta.LayerFeatures) {
		return nil, 0, fmt.Errorf("layer %s not in order", layerName)
	}
	feature := meta.LayerFeatures[idx]
	if feature == "" {
		return nil, 0, fmt.Errorf("layer %s has no feature", layerName)
	}
	blob, err := layerBlobAccess(imageTarPath, layerName, true)
	if err != nil {
		return nil, 0, err
	}
	ix := parseFeatureLayerBlob(blob, feature)
	if ix == nil {
		_ = blob.close()
		return nil, 0, fmt.Errorf("parse %s table of layer %s", feature, layerName)
	}
	entries := ix.byName[entryName]
	if len(entries) == 0 {
		_ = blob.close()
		return nil, 0, fmt.Errorf("entry %s not in layer %s", entryName, layerName)
	}
	e := entries[0]
	switch feature {
	case LayerFeatureEstargz:
		data, err := readEstargzEntry(blob, ix, e)
		if err != nil {
			_ = blob.close()
			return nil, 0, err
		}
		return &blobReadCloser{r: bytes.NewReader(data), close: blob.close}, e.hdr.Size, nil
	case LayerFeatureZstdChunked:
		data, err := readZstdChunkedEntry(blob, ix, e)
		if err != nil {
			_ = blob.close()
			return nil, 0, err
		}
		return &blobReadCloser{r: bytes.NewReader(data), close: blob.close}, e.hdr.Size, nil
	}
	_ = blob.close()
	return nil, 0, fmt.Errorf("unknown layer feature %q", feature)
}

// parseFeatureLayerBlob is parseFeatureLayer over an already-open blob.
func parseFeatureLayerBlob(blob *layerBlob, feature string) *featureLayerIndex {
	switch feature {
	case LayerFeatureEstargz:
		ix, err := parseEstargzTOC(blob)
		if err != nil {
			return nil
		}
		return ix
	case LayerFeatureZstdChunked:
		ix, err := parseZstdChunkedManifest(blob)
		if err != nil {
			return nil
		}
		return ix
	}
	return nil
}

type blobReadCloser struct {
	r     *bytes.Reader
	close func() error
}

func (b *blobReadCloser) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *blobReadCloser) Close() error               { return b.close() }

// featureLayerSHA256 digests a feature layer's payload ranges the way verify
// would see them; kept for future use by verify's fast path.
func featureLayerSHA256(blob *layerBlob, ix *featureLayerIndex, entries []*featureEntry) string {
	h := sha256.New()
	for _, e := range entries {
		var data []byte
		var err error
		switch ix.feature {
		case LayerFeatureEstargz:
			data, err = readEstargzEntry(blob, ix, e)
		case LayerFeatureZstdChunked:
			data, err = readZstdChunkedEntry(blob, ix, e)
		}
		if err != nil {
			return ""
		}
		h.Write(data)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// layerFeature reports the feature of one layer, or "" when unknown.
func layerFeature(meta *types.ImageMetadata, layerName string) string {
	for i, n := range meta.LayerOrder {
		if n == layerName && i < len(meta.LayerFeatures) {
			return meta.LayerFeatures[i]
		}
	}
	return ""
}

// applyFeatureLayerEntries extracts the selected entries of one feature layer
// from its table and compressed ranges. It reports an error when anything is
// off (the caller then streams the layer instead), so the fallback stays
// byte-identical.
func applyFeatureLayerEntries(plan *extractPlan, layerName, feature string, buf []byte, dirs *[]fsutil.DirMetadata, count *int) error {
	nodes := plan.byLayer[layerName]
	if len(nodes) == 0 {
		return nil
	}
	blob, err := layerBlobAccess(plan.imageTarPath, layerName, true)
	if err != nil {
		return err
	}
	defer blob.close()
	ix := parseFeatureLayerBlob(blob, feature)
	if ix == nil {
		return fmt.Errorf("parse %s table of layer %s", feature, layerName)
	}

	for clean, node := range nodes {
		if fsview.IsDeviceKind(node.Kind) {
			continue // devices/FIFOs are not recreated, matching the streaming path
		}
		entries := ix.byName[clean]
		if len(entries) == 0 {
			return fmt.Errorf("entry %s not in layer %s", clean, layerName)
		}
		e := entries[0]
		hdr := e.hdr
		var payload io.Reader = bytes.NewReader(nil)
		switch node.Kind {
		case fsview.KindFile:
			var data []byte
			switch feature {
			case LayerFeatureEstargz:
				data, err = readEstargzEntry(blob, ix, e)
			case LayerFeatureZstdChunked:
				data, err = readZstdChunkedEntry(blob, ix, e)
			default:
				err = fmt.Errorf("unknown feature %q", feature)
			}
			if err != nil {
				return err
			}
			payload = bytes.NewReader(data)
		}
		if err := applyNode(plan, node, hdr, payload, buf, dirs); err != nil {
			return err
		}
		*count++
	}
	return nil
}
