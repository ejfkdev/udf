package image

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/ejfkdev/udf/cachedir"
	"github.com/ejfkdev/udf/fsview"
	"github.com/ejfkdev/udf/gzipidx"
	arch "github.com/ejfkdev/udf/image/archive"
	"github.com/ejfkdev/udf/zstdidx"
)

// minIndexedArchiveSize is the compressed size from which a random-access
// index pays off: building it costs one extra pass, after which every layer
// read is a few hundred milliseconds instead of a full decompression.
const minIndexedArchiveSize = 64 << 20

// indexedOuter is the random-access view of a compressed tar archive: the member
// directory plus reads from an uncompressed offset. Both index backends
// implement it, so the rest of the package does not care whether the archive is
// gzip (checkpoints inside the deflate stream) or zstd (restarts at frame
// boundaries).
type indexedOuter interface {
	// Entries is the tar directory, sorted by name.
	Entries() []gzipidx.Entry
	// Lookup finds a member by exact name.
	Lookup(name string) (gzipidx.Entry, bool)
	// ReadAt returns a reader positioned at an uncompressed offset.
	ReadAt(outOff int64) (io.ReadCloser, error)
	Close() error
}

// gzipOuter adapts the gzip index reader.
type gzipOuter struct{ *gzipidx.Reader }

func (g *gzipOuter) Entries() []gzipidx.Entry { return g.Index().Entries }
func (g *gzipOuter) Lookup(name string) (gzipidx.Entry, bool) {
	return g.Index().Lookup(name)
}

// zstdOuter adapts the zstd frame index reader.
type zstdOuter struct{ *zstdidx.Reader }

func (z *zstdOuter) Entries() []gzipidx.Entry { return z.Index().Entries }
func (z *zstdOuter) Lookup(name string) (gzipidx.Entry, bool) {
	return z.Index().Lookup(name)
}

// indexableFormat reports whether a random-access index applies to this input,
// and which one: a large gzip or zstd compressed tar. The cheap checks come
// first so the (costly) index build only happens where it can help.
func indexableFormat(imageTarPath string) (string, bool) {
	st, err := os.Stat(imageTarPath)
	if err != nil || st.Size() < minIndexedArchiveSize {
		return "", false
	}
	format, err := arch.Detect(imageTarPath)
	if err != nil {
		return "", false
	}
	switch format {
	case "tar.gz":
		return "gzip", true
	case "tar.zst":
		return "zstd", true
	}
	return "", false
}

func indexableImage(imageTarPath string) bool {
	_, ok := indexableFormat(imageTarPath)
	return ok
}

// indexCachePath is where the index for one archive lives; the key carries the
// format so a gzip and a zstd index never collide.
func indexCachePath(imageTarPath, format string) string {
	return filepath.Join(cachedir.IndexDir(), cacheKeyFor(imageTarPath, format+"idx")+".idx")
}

// loadImageIndex returns the cached index for an image archive if one exists.
func loadImageIndex(imageTarPath string) (indexedOuter, bool) {
	format, ok := indexableFormat(imageTarPath)
	if !ok {
		return nil, false
	}
	path := indexCachePath(imageTarPath, format)
	switch format {
	case "gzip":
		ix, err := gzipidx.Load(path)
		if err != nil || !ix.Usable() {
			return nil, false
		}
		r, err := gzipidx.OpenReader(imageTarPath, ix)
		if err != nil {
			return nil, false
		}
		return &gzipOuter{r}, true
	case "zstd":
		ix, err := zstdidx.Load(path)
		if err != nil || !ix.Usable() {
			return nil, false
		}
		r, err := zstdidx.OpenReader(imageTarPath, ix)
		if err != nil {
			return nil, false
		}
		return &zstdOuter{r}, true
	}
	return nil, false
}

// ensureImageIndex returns the index for an image archive, building and
// caching it when missing. Callers use it for commands whose work is
// dominated by random layer access (cp, cat, extract).
func ensureImageIndex(imageTarPath string) (indexedOuter, bool) {
	format, ok := indexableFormat(imageTarPath)
	if !ok {
		return nil, false
	}
	idxDir := cachedir.IndexDir()
	path := indexCachePath(imageTarPath, format)

	if r, ok := loadImageIndex(imageTarPath); ok {
		return r, true
	}
	if format == "zstd" {
		// The zstd index carries the member directory but no layer capture:
		// zstd-compressed docker-save archives are read through the index,
		// which restarts at a frame boundary.
		ix, err := zstdidx.Build(imageTarPath)
		if err != nil || !ix.Usable() {
			return nil, false
		}
		if err := os.MkdirAll(idxDir, 0o755); err == nil {
			_ = ix.Save(path)
		}
		r, err := zstdidx.OpenReader(imageTarPath, ix)
		if err != nil {
			return nil, false
		}
		return &zstdOuter{r}, true
	}

	// While scanning, also collect the directories of the members that are
	// themselves tars: they are the layers, and the caller usually wants them
	// right away (to list or extract the merged rootfs). Parsing them from the
	// captured header bytes avoids decompressing every layer a second time.
	// Members that cannot be captured faithfully simply stay absent and are
	// read through the index as before.
	captured := make(map[string]*fsview.ParsedLayer)
	nested := &gzipidx.NestedOptions{
		Members: func(_ string, size int64, head []byte) bool {
			return size >= minNestedMemberSize && gzipidx.LooksLikeTarHeader(head)
		},
		Headers: func(name string, headers []*tar.Header) error {
			captured[name] = fsview.NewParsedLayer(headers)
			return nil
		},
	}

	ix, err := gzipidx.Build(imageTarPath, gzipidx.BuildOptions{Nested: nested})
	if err != nil || !ix.Usable() {
		return nil, false
	}
	if len(captured) > 0 {
		capturedLayers.Store(cacheKeyFor(imageTarPath, "gzipidx"), captured)
	}
	if err := os.MkdirAll(idxDir, 0o755); err == nil {
		_ = ix.Save(path)
	}
	r, err := gzipidx.OpenReader(imageTarPath, ix)
	if err != nil {
		return nil, false
	}
	return &gzipOuter{r}, true
}

// minNestedMemberSize is the smallest tar member worth capturing as a nested
// tar stream: below it there is no room even for a header and a terminator.
const minNestedMemberSize = 1024

// capturedLayers holds the layer directories that the index build captured in
// this process, keyed like the index cache. Only the command that built the
// index has them — every later command reads the layers through the index —
// and they are deliberately not persisted: the index stays small.
var capturedLayers sync.Map // string -> map[string]*fsview.ParsedLayer

// capturedLayerDirectories returns the layers of layerOrder when this process
// captured all of them while building the index.
func capturedLayerDirectories(imageTarPath string, layerOrder []string) (map[string]*fsview.ParsedLayer, bool) {
	val, ok := capturedLayers.Load(cacheKeyFor(imageTarPath, "gzipidx"))
	if !ok {
		return nil, false
	}
	all, ok := val.(map[string]*fsview.ParsedLayer)
	if !ok {
		return nil, false
	}
	for _, name := range layerOrder {
		if _, ok := all[name]; !ok {
			return nil, false
		}
	}
	return all, true
}

// openIndexedLayer opens a layer's stored bytes through the index: the
// decompressor restarts at the nearest checkpoint instead of decoding the
// whole archive.
func openIndexedLayer(r indexedOuter, layerName string) (io.ReadCloser, error) {
	entry, ok := r.Lookup(layerName)
	if !ok {
		return nil, fmt.Errorf("layer %s not found in index", layerName)
	}
	return r.ReadAt(entry.OutOff)
}

// parseLayersFromIndex reads and parses every layer through the index, in
// parallel: each layer decodes only its own span.
func parseLayersFromIndex(r indexedOuter, layerOrder []string) (map[string]*fsview.ParsedLayer, bool) {
	layers := make(map[string]*fsview.ParsedLayer, len(layerOrder))
	var mu sync.Mutex
	workers := runtime.NumCPU()
	if workers > len(layerOrder) {
		workers = len(layerOrder)
	}
	if workers < 1 {
		return nil, false
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	ok := true
	for _, name := range layerOrder {
		if _, dup := layers[name]; dup {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(layerName string) {
			defer wg.Done()
			defer func() { <-sem }()
			src, err := openIndexedLayer(r, layerName)
			if err != nil {
				mu.Lock()
				ok = false
				mu.Unlock()
				return
			}
			defer src.Close()
			parsed, err := fsview.ParseLayer(src)
			if err != nil {
				mu.Lock()
				ok = false
				mu.Unlock()
				return
			}
			mu.Lock()
			layers[layerName] = parsed
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	if !ok || len(layers) != len(layerOrder) {
		return nil, false
	}
	return layers, true
}

// indexedLayerSource ties a layer reader to the index reader feeding it, so
// closing the layer also releases the index.
type indexedLayerSource struct {
	layer io.ReadCloser
	idx   indexedOuter
}

func (s *indexedLayerSource) Read(p []byte) (int, error) { return s.layer.Read(p) }

func (s *indexedLayerSource) Close() error {
	err := s.layer.Close()
	_ = s.idx.Close()
	return err
}

// readOuterRange reads a byte range of the uncompressed stream through an
// index: the reader is positioned at the start and only the range is consumed.
func readOuterRange(r indexedOuter, off, size int64) ([]byte, error) {
	rc, err := r.ReadAt(off)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	buf := make([]byte, size)
	if _, err := io.ReadFull(rc, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
