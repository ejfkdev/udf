package image

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// Layer feature sniffing: some layers carry seekable-compression metadata in
// their compressed bytes that a plain gzip/zstd reader ignores. Detecting it
// costs one read of the layer's last few dozen bytes, so it is best-effort —
// layers in archives without a cheap random-access path are simply not
// reported.
const (
	// LayerFeatureEstargz: a stargz-snapshotter eStargz blob (seekable gzip
	// with a TOC member and a 51-byte footer carrying the TOC offset).
	LayerFeatureEstargz = "estargz"
	// LayerFeatureZstdChunked: a containers/storage zstd:chunked blob (per-file
	// zstd frames plus a metadata manifest in skippable frames).
	LayerFeatureZstdChunked = "zstd-chunked"
)

// estargzFooterSize is the fixed size of an eStargz footer: one gzip member
// whose extra field holds the TOC offset (16 hex chars) and the "STARGZ"
// magic (stargz-snapshotter estargz/gzip.go).
const estargzFooterSize = 51

// zstdChunkedTailSize is the trailing metadata of a zstd:chunked blob: one
// skippable frame (8-byte header) holding a 64-byte footer whose last 8 bytes
// are the frame magic (containers/storage pkg/chunked, "GNUlInUx").
const zstdChunkedTailSize = 8 + 64

var zstdChunkedFrameMagic = [8]byte{0x47, 0x4e, 0x55, 0x6c, 0x49, 0x6e, 0x55, 0x78}

// sniffLayerTail classifies a layer from the tail of its compressed bytes.
// An empty result means "nothing recognized"; both formats stay readable as
// plain gzip/zstd, so an unrecognized tail is not an error.
func sniffLayerTail(tail []byte) string {
	if isEstargzFooter(tail) {
		return LayerFeatureEstargz
	}
	if isZstdChunkedTail(tail) {
		return LayerFeatureZstdChunked
	}
	return ""
}

// isEstargzFooter parses the footer gzip member: FEXTRA with SI1='S' SI2='G',
// a 22-byte subfield of 16 hex digits followed by "STARGZ".
func isEstargzFooter(tail []byte) bool {
	if len(tail) != estargzFooterSize {
		return false
	}
	zr, err := gzip.NewReader(bytes.NewReader(tail))
	if err != nil {
		return false
	}
	defer zr.Close()
	extra := zr.Header.Extra
	if len(extra) != 4+22 || extra[0] != 'S' || extra[1] != 'G' {
		return false
	}
	subfield := extra[4:]
	if string(subfield[16:]) != "STARGZ" {
		return false
	}
	_, err = strconv.ParseUint(string(subfield[:16]), 16, 64)
	return err == nil
}

// isZstdChunkedTail parses the final skippable frame: magic 0x184D2A50, a
// 64-byte payload whose last 8 bytes are the zstd:chunked frame magic.
func isZstdChunkedTail(tail []byte) bool {
	if len(tail) != zstdChunkedTailSize {
		return false
	}
	if binary.LittleEndian.Uint32(tail[0:4]) != 0x184D2A50 {
		return false
	}
	if binary.LittleEndian.Uint32(tail[4:8]) != 64 {
		return false
	}
	return bytes.Equal(tail[zstdChunkedTailSize-8:], zstdChunkedFrameMagic[:])
}

// sniffLayerFeatures classifies every layer of the image at path by reading
// its compressed tail. It mirrors meta.LayerOrder's order. The expensive
// resources it needs are resolved once for the whole pass: the tar member
// table (one header-chain walk) and the outer index (one load).
func sniffLayerFeatures(path string, layerNames []string) []string {
	out := make([]string, len(layerNames))
	// One walk for a plain tar; also its cheap "is this a tar" verdict. The
	// walk only wins when it actually located the members — a directory (OCI
	// layout) or unreadable fd reads as an empty archive and must fall
	// through to the per-layer transports.
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		if st, err := f.Stat(); err == nil && !st.IsDir() {
			if members, ok := locateTarMembers(f, st.Size()); ok && len(members) > 0 {
				found := false
				for i, name := range layerNames {
					m, ok := members[name]
					if !ok || m.size < zstdChunkedTailSize {
						continue
					}
					out[i] = sniffTailAt(f, m, estargzFooterSize)
					if out[i] == "" {
						out[i] = sniffTailAt(f, m, zstdChunkedTailSize)
					}
					if out[i] != "" {
						found = true
					}
				}
				if found {
					return out
				}
			}
		}
	}
	// Not a plain tar: OCI layout directory files, then an already-built index
	// (loaded once, never built here).
	var ix indexedOuter
	defer func() {
		if ix != nil {
			ix.Close()
		}
	}()
	for i, name := range layerNames {
		tail, ok := readMemberTailWith(path, name, estargzFooterSize, &ix)
		if ok {
			if fv := sniffLayerTail(tail); fv != "" {
				out[i] = fv
				continue
			}
			if tail2, ok := readMemberTailWith(path, name, zstdChunkedTailSize, &ix); ok {
				out[i] = sniffLayerTail(tail2)
			}
		}
	}
	return out
}

// sniffTailAt reads n bytes at the end of a tar member from f.
func sniffTailAt(f *os.File, m tarMember, n int64) string {
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, m.start+m.size-n); err != nil {
		return ""
	}
	return sniffLayerTail(buf)
}

// readMemberTailWith is readMemberTail with a shared index handle.
func readMemberTailWith(path, name string, n int64, ix *indexedOuter) ([]byte, bool) {
	if tail, ok := readFileTail(path, name, n); ok {
		return tail, true
	}
	if *ix == nil {
		if loaded, ok := loadImageIndex(path); ok {
			*ix = loaded
		}
	}
	if *ix != nil {
		e, ok := (*ix).Lookup(name)
		if ok && e.Size >= n {
			rc, err := (*ix).ReadAt(e.OutOff + e.Size - n)
			if err == nil {
				defer rc.Close()
				buf := make([]byte, n)
				if _, err := io.ReadFull(rc, buf); err == nil {
					return buf, true
				}
			}
		}
	}
	return nil, false
}

// readFileTail covers OCI layout directories: layer blobs are plain files
// under the layout root.
func readFileTail(path, name string, n int64) ([]byte, bool) {
	full := filepath.Join(path, name)
	st, err := os.Stat(full)
	if err != nil || st.IsDir() || st.Size() < n {
		return nil, false
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, st.Size()-n); err != nil {
		return nil, false
	}
	return buf, true
}

func isZeroBlock(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// hdrSize parses a tar header's octal size field.
func hdrSize(h []byte) (int64, error) {
	return strconv.ParseInt(string(bytes.TrimRight(h[124:136], " \x00")), 8, 64)
}

// tarName extracts a tar header's name field.
func tarName(h []byte) string {
	if i := bytes.IndexByte(h[:100], 0); i >= 0 {
		return string(h[:i])
	}
	return string(h[:100])
}
