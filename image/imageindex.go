package image

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/ejfkdev/udf/fsview"
	"github.com/ejfkdev/udf/gzipidx"
	arch "github.com/ejfkdev/udf/image/archive"
)

// minIndexedArchiveSize is the compressed size from which a random-access
// index pays off: building it costs one extra pass, after which every layer
// read is a few hundred milliseconds instead of a full decompression.
const minIndexedArchiveSize = 64 << 20

// indexableImage reports whether a random-access index applies to this input:
// a large single-member gzip tar. The cheap checks come first so the (costly)
// index build only happens where it can help.
func indexableImage(imageTarPath string) bool {
	st, err := os.Stat(imageTarPath)
	if err != nil || st.Size() < minIndexedArchiveSize {
		return false
	}
	format, err := arch.Detect(imageTarPath)
	if err != nil || format != "tar.gz" {
		return false
	}
	return true
}

// loadImageIndex returns the cached index for an image archive if one exists.
func loadImageIndex(imageTarPath string) (*gzipidx.Reader, bool) {
	if !indexableImage(imageTarPath) {
		return nil, false
	}
	dir := cacheDir()
	if dir == "" {
		return nil, false
	}
	path := filepath.Join(dir, "idx", cacheKeyFor(imageTarPath, "gzipidx")+".idx")
	ix, err := gzipidx.Load(path)
	if err != nil || !ix.Usable() {
		return nil, false
	}
	r, err := gzipidx.OpenReader(imageTarPath, ix)
	if err != nil {
		return nil, false
	}
	return r, true
}

// ensureImageIndex returns the index for an image archive, building and
// caching it when missing. Callers use it for commands whose work is
// dominated by random layer access (cp, cat, extract).
func ensureImageIndex(imageTarPath string) (*gzipidx.Reader, bool) {
	if !indexableImage(imageTarPath) {
		return nil, false
	}
	dir := cacheDir()
	if dir == "" {
		return nil, false
	}
	idxDir := filepath.Join(dir, "idx")
	path := filepath.Join(idxDir, cacheKeyFor(imageTarPath, "gzipidx")+".idx")

	if ix, err := gzipidx.Load(path); err == nil && ix.Usable() {
		if r, err := gzipidx.OpenReader(imageTarPath, ix); err == nil {
			return r, true
		}
	}

	ix, err := gzipidx.Build(imageTarPath, gzipidx.BuildOptions{})
	if err != nil || !ix.Usable() {
		return nil, false
	}
	if err := os.MkdirAll(idxDir, 0o755); err == nil {
		_ = ix.Save(path)
	}
	r, err := gzipidx.OpenReader(imageTarPath, ix)
	if err != nil {
		return nil, false
	}
	return r, true
}

// openIndexedLayer opens a layer's stored bytes through the index: the
// decompressor restarts at the nearest checkpoint instead of decoding the
// whole archive.
func openIndexedLayer(r *gzipidx.Reader, layerName string) (io.ReadCloser, error) {
	entry, ok := r.Index().Lookup(layerName)
	if !ok {
		return nil, fmt.Errorf("layer %s not found in index", layerName)
	}
	return r.ReadAt(entry.OutOff)
}

// parseLayersFromIndex reads and parses every layer through the index, in
// parallel: each layer decodes only its own span.
func parseLayersFromIndex(r *gzipidx.Reader, layerOrder []string) (map[string]*fsview.ParsedLayer, bool) {
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
	idx   *gzipidx.Reader
}

func (s *indexedLayerSource) Read(p []byte) (int, error) { return s.layer.Read(p) }

func (s *indexedLayerSource) Close() error {
	err := s.layer.Close()
	_ = s.idx.Close()
	return err
}
