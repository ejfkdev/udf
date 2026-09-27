package image

import (
	"fmt"
	"io"

	arch "github.com/ejfkdev/udf/image/archive"
	"github.com/ejfkdev/udf/layer"
)

// layerStreamSource serves layer contents from a single sequential pass over
// the image archive. Reaching a far entry in a compressed archive costs a
// full decompression, so merging or extracting N layers through random-access
// Open calls is O(N x stream) — this source pays the pass once.
//
// Requests must arrive in archive order and each returned reader must be
// consumed before the next request (which is how the layer merge and the
// layered extract walk the image). Anything else — a layer already passed, an
// archive without sequential support — falls back to random access.
type layerStreamSource struct {
	archive arch.Archive
	seq     arch.SequentialReader
	served  map[string]bool
	dead    bool // sequential pass unusable; use random access from here on
}

// newLayerStreamSource returns a source for archive, or nil when the archive
// has no sequential reader (callers then keep using Open directly).
func newLayerStreamSource(archive arch.Archive) *layerStreamSource {
	seq := arch.SequentialOf(archive)
	if seq == nil {
		return nil
	}
	return &layerStreamSource{archive: archive, seq: seq, served: map[string]bool{}}
}

func (s *layerStreamSource) Close() {
	if s.seq != nil {
		s.seq.Close()
	}
}

// OpenLayer opens the named layer and wraps it with the layer decompressor.
// The returned reader stays valid until the next OpenLayer/OpenRaw call.
func (s *layerStreamSource) OpenLayer(name string) (io.Reader, func(), error) {
	raw, closeFn, err := s.OpenRaw(name)
	if err != nil {
		return nil, func() {}, err
	}
	r, closeLayer, err := layer.OpenLayerReader(raw)
	if err != nil {
		closeFn()
		return nil, func() {}, fmt.Errorf("open layer %s: %w", name, err)
	}
	return r, func() { closeLayer(); closeFn() }, nil
}

// OpenRaw returns the layer's stored bytes (no decompression), for callers
// that run their own layer reader over it (layer.ApplyLayer).
func (s *layerStreamSource) OpenRaw(name string) (io.Reader, func(), error) {
	if s == nil {
		return nil, func() {}, fmt.Errorf("no layer source")
	}
	if s.dead || s.served[name] {
		return s.randomAccessRaw(name)
	}

	for {
		entryName, _, r, err := s.seq.Next()
		if err != nil {
			// Entry not present later in the stream, or the pass failed: give
			// up on it and let random access serve this and later requests
			// (it reports a proper error when the name truly is absent).
			s.dead = true
			s.seq.Close()
			s.seq = nil
			return s.randomAccessRaw(name)
		}
		if entryName != name {
			continue
		}
		s.served[name] = true
		return r, func() {}, nil
	}
}

func (s *layerStreamSource) randomAccessRaw(name string) (io.Reader, func(), error) {
	rc, _, err := s.archive.Open(name)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open layer %s: %w", name, err)
	}
	return rc, func() { _ = rc.Close() }, nil
}
